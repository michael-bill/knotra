//go:build unix

package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

func TestRuntimeForeachProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RUNTIME_RECOVERY") != "1" {
		t.Skip("set KNOTRA_TEST_RUNTIME_RECOVERY=1 for foreach Runtime SIGKILL checks")
	}
	for _, mode := range []string{"child_creation_before_commit", "child_before_commit", "collection_before_commit", "collection_after_first_page", "parent_before_commit"} {
		t.Run(mode, func(t *testing.T) {
			r := newTestRuntime(t)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": `{"answer":"confirmed-prefix"}`}, "done": true, "done_reason": "stop"})
			}))
			defer server.Close()
			plan := foreachPlan(server.URL, false)
			plan.Profile.Spec.Limits.Timeout = "5m"
			plan.Profile.Spec.Limits.MaxNodeInstances = 200
			parent := plan.Pipelines[plan.Root].Spec.Nodes["map"]
			parent.Dependencies = []string{"before"}
			parent.Execution.Timeout = "3m"
			count := 130
			if mode == "child_creation_before_commit" || mode == "child_before_commit" {
				count = 3
			}
			items := make([]int, count)
			for i := range items {
				items[i] = count - i
			}
			values, err := json.Marshal(items)
			if err != nil {
				t.Fatal(err)
			}
			input := parent.Inputs["items"]
			input.Bind = &contract.Binding{Value: values}
			parent.Inputs["items"] = input
			parent.Foreach.Concurrency = 8
			parent.Foreach.Body.Nodes = nil
			parent.Foreach.Body.Outputs = map[string]contract.Port{"result": compositePort(`{"type":"integer"}`, "inputs.item")}
			plan.Pipelines[plan.Root].Spec.Nodes["map"] = parent
			plan.Pipelines[plan.Root].Spec.Nodes["before"] = deliveryPlan(server.URL).Pipelines[plan.Root].Spec.Nodes["a"]
			id := admitRuntimeRun(t, r, plan)
			parentID := execution.StableID("n", id+"/root/map")
			directory := t.TempDir()
			dsn, err := url.Parse(os.Getenv("KNOTRA_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			query := dsn.Query()
			query.Set("search_path", r.Store.Pool.Config().ConnConfig.RuntimeParams["search_path"])
			application := "knotra-control-crash-" + id
			query.Set("application_name", application)
			dsn.RawQuery = query.Encode()
			barrier, err := pgx.Connect(ctx, dsn.String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = barrier.Close(context.Background()) }()
			if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273646)"); err != nil {
				t.Fatal(err)
			}
			table, when, event := "knotra_execution_controls", "NEW.collection_position>OLD.collection_position AND OLD.collection_position=0", "UPDATE"
			position := 0
			switch mode {
			case "child_creation_before_commit":
				table, when, event = "knotra_execution_graphs", "NEW.parent_instance_id<>'' AND NEW.iteration_index=0", "INSERT"
			case "child_before_commit":
				table, when = "knotra_execution_graphs", "NEW.parent_instance_id<>'' AND NEW.iteration_index=0 AND NEW.state='succeeded' AND OLD.state<>'succeeded'"
			case "collection_after_first_page":
				when, position = "NEW.collection_position>OLD.collection_position AND OLD.collection_position=64", 64
			case "parent_before_commit":
				table, when, position = "knotra_execution_nodes", "NEW.node_id='map' AND NEW.state='succeeded' AND OLD.state<>'succeeded'", count
			}
			if _, err := r.Store.Pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION control_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273646); RETURN NEW; END $$; CREATE TRIGGER control_crash_barrier BEFORE %s ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION control_crash_barrier()`, event, table, when)); err != nil {
				t.Fatal(err)
			}
			child := startControlRuntimeProcess(t, ctx, dsn.String(), directory, id, mode)
			await(t, 90*time.Second, func() (bool, error) {
				var waiting bool
				err := r.Store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory' AND pid<>pg_backend_pid())`, application).Scan(&waiting)
				return waiting, err
			})
			var deadline time.Time
			var next, collected, active, succeeded, failed int
			if err := r.Store.Pool.QueryRow(ctx, `SELECT n.deadline,c.next_position,c.collection_position,c.active_children,c.succeeded_children,c.failed_children FROM knotra_execution_controls c JOIN knotra_execution_nodes n ON n.run_id=c.run_id AND n.id=c.instance_id WHERE c.run_id=$1 AND c.instance_id=$2`, id, parentID).Scan(&deadline, &next, &collected, &active, &succeeded, &failed); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || collected != position || active+succeeded+failed != next || failed != 0 {
				t.Fatalf("boundary calls=%d position=%d next=%d children=%d/%d/%d", calls.Load(), collected, next, active, succeeded, failed)
			}
			switch mode {
			case "child_creation_before_commit":
				var children int
				if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_graphs WHERE run_id=$1 AND parent_instance_id=$2", id, parentID).Scan(&children); err != nil || children != 0 || next != 0 || active != 0 || succeeded != 0 {
					t.Fatalf("child creation committed before barrier: children=%d next=%d active=%d succeeded=%d error=%v", children, next, active, succeeded, err)
				}
			case "child_before_commit":
				var state string
				if err := r.Store.Pool.QueryRow(ctx, "SELECT state FROM knotra_execution_graphs WHERE run_id=$1 AND parent_instance_id=$2 AND iteration_index=0", id, parentID).Scan(&state); err != nil || state == "succeeded" {
					t.Fatalf("child completion committed before barrier: state=%s error=%v", state, err)
				}
			default:
				if next != count || succeeded != count || active != 0 {
					t.Fatalf("parent barrier did not follow committed child completion: next=%d succeeded=%d active=%d", next, succeeded, active)
				}
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = child.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("child did not exit from a signal: %v", err)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child was not killed with SIGKILL: %v", err)
			}
			if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273646)"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Store.Pool.Exec(ctx, "DROP TRIGGER control_crash_barrier ON "+table); err != nil {
				t.Fatal(err)
			}
			var afterNext, afterPosition, afterActive, afterSucceeded int
			if err := r.Store.Pool.QueryRow(ctx, "SELECT next_position,collection_position,active_children,succeeded_children FROM knotra_execution_controls WHERE run_id=$1 AND instance_id=$2", id, parentID).Scan(&afterNext, &afterPosition, &afterActive, &afterSucceeded); err != nil || afterNext != next || afterPosition != collected || afterActive != active || afterSucceeded != succeeded {
				t.Fatalf("SIGKILL committed interrupted progress: next=%d position=%d active=%d succeeded=%d error=%v", afterNext, afterPosition, afterActive, afterSucceeded, err)
			}
			replacement := startControlRuntimeProcess(t, ctx, dsn.String(), directory, id, "recover")
			if err := replacement.Wait(); err != nil {
				log, _ := os.ReadFile(filepath.Join(directory, "recover.log"))
				t.Fatalf("replacement failed: %v\n%s", err, log)
			}
			run, err := r.Store.Run(ctx, id)
			if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != string(values) {
				t.Fatalf("recovered status=%s outputs=%s error=%v", run.Status, run.Outputs["result"], err)
			}
			var children, distinctPositions, attempts, budget, slots, successes, workers int
			var finalDeadline time.Time
			if err := r.Store.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_graphs WHERE run_id=$1 AND parent_instance_id=$2 AND state='succeeded'),
				(SELECT count(DISTINCT iteration_index) FROM knotra_execution_graphs WHERE run_id=$1 AND parent_instance_id=$2),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND number=1 AND state='completed'),
				(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
				(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
				(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->>'message'='succeeded'),
				(SELECT count(*) FROM knotra_workers),
				(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2),
				c.next_position,c.collection_position,c.active_children,c.succeeded_children,c.failed_children
				FROM knotra_execution_controls c WHERE c.run_id=$1 AND c.instance_id=$2`, id, parentID).Scan(&children, &distinctPositions, &attempts, &budget, &slots, &successes, &workers, &finalDeadline, &next, &collected, &active, &succeeded, &failed); err != nil {
				t.Fatal(err)
			}
			if children != count || distinctPositions != count || calls.Load() != 1 || attempts != 1 || budget != 1 || slots != 0 || successes != 2 || workers != 2 || !deadline.Equal(finalDeadline) || next != count || collected != count || active != 0 || succeeded != count || failed != 0 {
				t.Fatalf("children=%d/%d calls=%d attempts=%d budget=%d slots=%d successes=%d workers=%d deadline=%v control=%d/%d/%d/%d/%d", children, distinctPositions, calls.Load(), attempts, budget, slots, successes, workers, deadline.Equal(finalDeadline), next, collected, active, succeeded, failed)
			}
			t.Logf("SIGKILL preserved committed child/page progress, %d ordered values, one physical call/debit and the original parent deadline", count)
		})
	}
}

func TestRuntimeLoopAndPipelineProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RUNTIME_RECOVERY") != "1" {
		t.Skip("set KNOTRA_TEST_RUNTIME_RECOVERY=1 for loop/pipeline Runtime SIGKILL checks")
	}
	for _, kind := range []string{"loop", "pipeline"} {
		modes := []string{"child_creation_before_commit", "child_before_commit", "parent_before_commit"}
		if kind == "loop" {
			modes = append(modes, "carry_before_commit", "carry_committed_before_child")
		}
		for _, mode := range modes {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				r := newTestRuntime(t)
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
				defer cancel()
				var calls [3]atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					var body struct{ Messages []struct{ Content string } }
					if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					var inputs struct{ Values map[string]int }
					for _, message := range body.Messages {
						if data, found := strings.CutPrefix(message.Content, "Knotra input context (data):\n"); found {
							if err := json.Unmarshal([]byte(data), &inputs); err != nil {
								t.Error(err)
								return
							}
						}
					}
					index, found := inputs.Values["index"]
					if !found || index < 0 || index >= len(calls) || len(inputs.Values) != 3 {
						t.Errorf("provider inputs=%v", inputs.Values)
						http.Error(w, "missing inputs", http.StatusBadRequest)
						return
					}
					calls[index].Add(1)
					content, err := json.Marshal(inputs.Values)
					if err != nil {
						t.Error(err)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": string(content)}, "done": true, "done_reason": "stop"})
				}))
				defer server.Close()
				plan := runtimePlan(server.URL)
				plan.Profile.Spec.Limits.Timeout = "5m"
				integer := `{"type":"integer"}`
				leaf := plan.Pipelines[plan.Root].Spec.Nodes["a"]
				leaf.Inputs, leaf.Outputs = map[string]contract.Port{}, map[string]contract.Port{}
				body := contract.Graph{Inputs: map[string]contract.Port{}, Nodes: map[string]contract.Node{}, Outputs: map[string]contract.Port{}}
				parent := contract.Node{Type: kind, Execution: contract.Execution{Timeout: "3m"}, Outputs: map[string]contract.Port{}}
				root := contract.Graph{Nodes: map[string]contract.Node{}, Outputs: map[string]contract.Port{}}
				for _, name := range []string{"left", "right", "index"} {
					body.Inputs[name] = compositePort(integer, "")
					leaf.Inputs[name] = compositePort(integer, "inputs."+name)
					leaf.Outputs[name], parent.Outputs[name] = compositePort(integer, ""), compositePort(integer, "")
					body.Outputs[name] = compositePort(integer, "nodes.work.outputs."+name)
					root.Outputs[name] = compositePort(integer, "nodes.control.outputs."+name)
				}
				body.Nodes["work"] = leaf
				count := 1
				if kind == "loop" {
					count = 3
					left, right := compositePort(integer, ""), compositePort(integer, "")
					left.Initial, left.Next = &contract.Binding{Value: json.RawMessage(`1`)}, &contract.Binding{From: "body.outputs.right"}
					right.Initial, right.Next = &contract.Binding{Value: json.RawMessage(`2`)}, &contract.Binding{From: "body.outputs.left"}
					parent.Loop = &contract.LoopNode{MaxIterations: count, Until: "iteration.index == 2", State: map[string]contract.Port{"left": left, "right": right},
						With: map[string]contract.Binding{"left": {From: "state.left"}, "right": {From: "state.right"}, "index": {Expr: "iteration.index"}}, Body: body}
					parent.Outputs["iterations"], parent.Outputs["termination"] = compositePort(integer, ""), compositePort(`{"type":"string"}`, "")
					root.Outputs["iterations"], root.Outputs["termination"] = compositePort(integer, "nodes.control.outputs.iterations"), compositePort(`{"type":"string"}`, "nodes.control.outputs.termination")
				} else {
					child := *plan.Pipelines[plan.Root]
					child.Spec.Graph = body
					child.Spec.Limits = contract.Limits{Timeout: "2m", MaxConcurrentNodes: 1, MaxModelCalls: 3, MaxNodeInstances: 10}
					plan.Pipelines["child.yaml"] = &child
					parent.Pipeline = &contract.PipelineNode{File: "child.yaml", Permissions: contract.Permissions{Models: []string{"model"}}}
					parent.Inputs = map[string]contract.Port{}
					for name, value := range map[string]string{"left": "1", "right": "2", "index": "0"} {
						port := compositePort(integer, "")
						port.Bind = &contract.Binding{Value: json.RawMessage(value)}
						parent.Inputs[name] = port
					}
				}
				root.Nodes["control"] = parent
				plan.Pipelines[plan.Root].Spec.Graph = root
				id := admitRuntimeRun(t, r, plan)
				parentID := execution.StableID("n", id+"/root/control")
				directory := t.TempDir()
				dsn, err := url.Parse(os.Getenv("KNOTRA_TEST_DATABASE_URL"))
				if err != nil {
					t.Fatal(err)
				}
				application := "knotra-sequential-crash-" + id
				query := dsn.Query()
				query.Set("search_path", r.Store.Pool.Config().ConnConfig.RuntimeParams["search_path"])
				query.Set("application_name", application)
				dsn.RawQuery = query.Encode()
				barrier, err := pgx.Connect(ctx, dsn.String())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273647)"); err != nil {
					t.Fatal(err)
				}
				table, when, event := "knotra_execution_graphs", "NEW.parent_instance_id IS NOT NULL", "INSERT"
				iteration, expectedCalls := 0, 0
				switch mode {
				case "child_before_commit":
					event, expectedCalls = "UPDATE", 1
					when += " AND NEW.state='succeeded' AND OLD.state<>'succeeded'"
					if kind == "loop" {
						when += " AND NEW.iteration_index=1"
						iteration, expectedCalls = 1, 2
					}
				case "parent_before_commit":
					table, when, event, expectedCalls = "knotra_execution_nodes", "NEW.node_id='control' AND NEW.state='succeeded' AND OLD.state<>'succeeded'", "UPDATE", count
					iteration = count - 1
				case "carry_before_commit":
					table, when, event, iteration, expectedCalls = "knotra_execution_controls", "NEW.iteration=2 AND OLD.iteration=1", "UPDATE", 1, 2
				case "carry_committed_before_child":
					when += " AND NEW.iteration_index=2"
					iteration, expectedCalls = 2, 2
				}
				if _, err := r.Store.Pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION sequential_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273647); RETURN NEW; END $$; CREATE TRIGGER sequential_crash_barrier BEFORE %s ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION sequential_crash_barrier()`, event, table, when)); err != nil {
					t.Fatal(err)
				}
				child := startControlRuntimeProcess(t, ctx, dsn.String(), directory, id, mode)
				await(t, 30*time.Second, func() (bool, error) {
					var waiting bool
					err := r.Store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory' AND pid<>pg_backend_pid())`, application).Scan(&waiting)
					return waiting, err
				})
				var before []byte
				var control execution.ControlRecord
				var deadline, started time.Time
				if err := r.Store.Pool.QueryRow(ctx, `SELECT to_jsonb(c),n.deadline,n.started_at FROM knotra_execution_controls c JOIN knotra_execution_nodes n ON n.run_id=c.run_id AND n.id=c.instance_id WHERE c.run_id=$1 AND c.instance_id=$2`, id, parentID).Scan(&before, &deadline, &started); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(before, &control); err != nil {
					t.Fatal(err)
				}
				if total := calls[0].Load() + calls[1].Load() + calls[2].Load(); total != int32(expectedCalls) || control.Iteration != iteration || control.FailedChildren != 0 {
					t.Fatalf("boundary calls=%d iteration=%d failures=%d want=%d/%d", total, control.Iteration, control.FailedChildren, expectedCalls, iteration)
				}
				if kind == "loop" {
					left, right := "1", "2"
					if iteration%2 == 1 {
						left, right = right, left
					}
					if string(control.Carry["left"].JSON) != left || string(control.Carry["right"].JSON) != right {
						t.Fatalf("carry at iteration %d=%+v", iteration, control.Carry)
					}
				}
				if err := child.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				err = child.Wait()
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("child did not exit from a signal: %v", err)
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || status.Signal() != syscall.SIGKILL {
					t.Fatalf("child was not killed with SIGKILL: %v", err)
				}
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273647)"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Store.Pool.Exec(ctx, "DROP TRIGGER sequential_crash_barrier ON "+table); err != nil {
					t.Fatal(err)
				}
				var after []byte
				if err := r.Store.Pool.QueryRow(ctx, "SELECT to_jsonb(c) FROM knotra_execution_controls c WHERE run_id=$1 AND instance_id=$2", id, parentID).Scan(&after); err != nil || string(after) != string(before) {
					t.Fatalf("SIGKILL changed committed control: before=%s after=%s error=%v", before, after, err)
				}
				replacement := startControlRuntimeProcess(t, ctx, dsn.String(), directory, id, "recover")
				if err := replacement.Wait(); err != nil {
					log, _ := os.ReadFile(filepath.Join(directory, "recover.log"))
					t.Fatalf("replacement failed: %v\n%s", err, log)
				}
				run, err := r.Store.Run(ctx, id)
				if err != nil || run.Status != "succeeded" || string(run.Outputs["left"]) != "1" || string(run.Outputs["right"]) != "2" || string(run.Outputs["index"]) != fmt.Sprint(count-1) {
					t.Fatalf("recovered status=%s outputs=%+v diagnostics=%+v error=%v", run.Status, run.Outputs, run.Diagnostics, err)
				}
				if kind == "loop" && (string(run.Outputs["iterations"]) != "3" || string(run.Outputs["termination"]) != `"condition"`) {
					t.Fatalf("loop termination changed: %+v", run.Outputs)
				}
				var graphs, succeeded, attempts, completed, budget, slots, successes, workers int
				var finalDeadline time.Time
				if err := r.Store.Pool.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM knotra_execution_graphs WHERE run_id=$1 AND parent_instance_id=$2),
					(SELECT count(*) FROM knotra_execution_graphs WHERE run_id=$1 AND parent_instance_id=$2 AND state='succeeded'),
					(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
					(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND number=1 AND state='completed' AND outcome IS NOT NULL),
					(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
					(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),
					(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->>'message'='succeeded'),
					(SELECT count(*) FROM knotra_workers),
					(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2)`, id, parentID).Scan(&graphs, &succeeded, &attempts, &completed, &budget, &slots, &successes, &workers, &finalDeadline); err != nil {
					t.Fatal(err)
				}
				if graphs != count || succeeded != count || attempts != count || completed != count || budget != count || slots != 0 || successes != count+1 || workers != 2 || !deadline.Equal(finalDeadline) {
					t.Fatalf("graphs=%d/%d attempts=%d/%d budget=%d slots=%d successes=%d workers=%d deadline=%v", graphs, succeeded, attempts, completed, budget, slots, successes, workers, deadline.Equal(finalDeadline))
				}
				for i := range calls {
					want := int32(0)
					if i < count {
						want = 1
					}
					if calls[i].Load() != want {
						t.Fatalf("iteration %d executed %d times, want %d", i, calls[i].Load(), want)
					}
				}
				if kind == "pipeline" {
					var childDeadline time.Time
					var childBudget, scopes int
					var models []string
					if err := r.Store.Pool.QueryRow(ctx, `SELECT g.deadline,jsonb_array_length(g.scopes),ARRAY(SELECT jsonb_array_elements_text(g.permissions->'models')),
						(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$2 AND kind='model') FROM knotra_execution_graphs g WHERE g.run_id=$1 AND g.parent_instance_id=$2`, id, parentID).Scan(&childDeadline, &scopes, &models, &childBudget); err != nil || !childDeadline.Equal(started.Add(2*time.Minute)) || scopes != 2 || len(models) != 1 || models[0] != "model" || childBudget != 1 {
						t.Fatalf("child deadline=%v scopes=%d permissions=%v budget=%d error=%v", childDeadline, scopes, models, childBudget, err)
					}
				}
				t.Logf("SIGKILL preserved %s child/carry state, original deadlines, %d physical calls/attempts/debits and one-time publication", kind, count)
			})
		}
	}
}

func startControlRuntimeProcess(t *testing.T, ctx context.Context, dsn, directory, runID, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeProcessCrashBoundaries$", "-test.timeout=3m")
	cmd.Env = append(os.Environ(), "KNOTRA_RUNTIME_CHILD=1", "KNOTRA_RUNTIME_MODE="+mode, "KNOTRA_RUNTIME_DSN="+dsn, "KNOTRA_RUNTIME_DIRECTORY="+directory, "KNOTRA_RUNTIME_RUN="+runID, "KNOTRA_RUNTIME_TIMEOUT=4m")
	log, err := os.Create(filepath.Join(directory, mode+".log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	_ = log.Close()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}
