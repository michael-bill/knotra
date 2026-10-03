package cli

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/spf13/cobra"
)

func (s *commandState) runCommand() *cobra.Command {
	var root, profile, definition, inputsFile, artifactsFile, key string
	var inputs, artifacts []string
	var wait, watch bool
	cmd := &cobra.Command{Use: "run [PIPELINE.yaml]", Short: "Admit and start an immutable pipeline run", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if (len(args) == 0) == (definition == "") {
			return fmt.Errorf("provide one pipeline file or --definition ID")
		}
		if profile == "" {
			return fmt.Errorf("--profile or KNOTRA_PROFILE is required")
		}
		if wait && watch {
			return fmt.Errorf("choose --wait or --watch")
		}
		in, err := s.object(inputsFile, inputs)
		if err != nil {
			return err
		}
		art, err := s.artifactBindings(artifactsFile, artifacts)
		if err != nil {
			return err
		}
		id := operationKey(key)
		engine := s.client()
		definitionID := definition
		if len(args) > 0 {
			pkg, err := loadPackage(args[0], root)
			if err != nil {
				return err
			}
			var result struct {
				Definition protocol.Definition `json:"definition"`
			}
			if err = engine.Command(cmd.Context(), "/definitions", map[string]any{"package": pkg}, definitionKey(id), &result); err != nil {
				return err
			}
			definitionID = result.Definition.ID
		}
		var result struct {
			Run protocol.Run `json:"run"`
		}
		if err = engine.Command(cmd.Context(), "/runs", map[string]any{"definitionId": definitionID, "profile": profile, "inputs": in, "artifacts": art}, id, &result); err != nil {
			return err
		}
		if !wait && !watch {
			return s.printRun(result.Run, id)
		}
		if !s.json {
			if _, err = fmt.Fprintf(s.options.Out, "Accepted run %s (operation %s).\n", result.Run.ID, id); err != nil {
				return err
			}
		}
		if watch {
			if err = s.watchRun(cmd.Context(), result.Run.ID, "", false); err != nil {
				return err
			}
		}
		final, err := s.waitRun(cmd.Context(), result.Run.ID)
		if err != nil {
			return err
		}
		if err = s.printRun(final, id); err != nil {
			return err
		}
		return runExit(final)
	}}
	f := cmd.Flags()
	f.StringVar(&root, "package-root", "", "Root of package-relative paths")
	f.StringVar(&profile, "profile", s.env("KNOTRA_PROFILE", ""), "Engine profile ID")
	f.StringVar(&definition, "definition", "", "Start an already published definition ID")
	f.StringVar(&key, "idempotency-key", "", "Stable operation ID for this exact run submission")
	f.BoolVar(&wait, "wait", false, "Wait for terminal status; disconnecting does not cancel the run")
	f.BoolVar(&watch, "watch", false, "Stream events until completion")
	inputFlags(cmd, &inputsFile, &inputs, &artifactsFile, &artifacts)
	return cmd
}

func (s *commandState) runsCommand() *cobra.Command {
	root := &cobra.Command{Use: "runs", Short: "Inspect and control pipeline runs"}
	var cursor string
	var all bool
	list := &cobra.Command{Use: "list", Short: "List runs", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		page, err := fetchPages[protocol.Run](cmd.Context(), s, "/runs", cursor, all)
		if err != nil {
			return err
		}
		if s.json {
			return s.printJSON(page)
		}
		rows := [][]string{}
		for _, run := range page.Items {
			rows = append(rows, []string{run.ID, run.Status, run.Profile, run.Title, run.CreatedAt.Format(time.RFC3339)})
		}
		if err = s.table([]string{"ID", "STATUS", "PROFILE", "TITLE", "CREATED"}, rows); err != nil {
			return err
		}
		return s.nextCursor(page.NextCursor)
	}}
	pageFlags(list, &cursor, &all)
	get := &cobra.Command{Use: "get RUN_ID", Short: "Show run inputs, outputs, instances and diagnostics", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		run, err := s.getRun(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return s.printRun(run, "")
	}}
	var fromStart bool
	var eventCursor string
	watch := &cobra.Command{Use: "watch RUN_ID", Short: "Follow durable events; Ctrl-C stops observation only", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if fromStart && eventCursor != "" {
			return fmt.Errorf("--from-start and --cursor are mutually exclusive")
		}
		if err := s.watchRun(cmd.Context(), args[0], eventCursor, fromStart); err != nil {
			return err
		}
		run, err := s.getRun(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return runExit(run)
	}}
	watch.Flags().BoolVar(&fromStart, "from-start", false, "Replay from the first retained event instead of saved cursor")
	watch.Flags().StringVar(&eventCursor, "cursor", "", "Resume after this explicit event ID")
	var timeout time.Duration
	wait := &cobra.Command{Use: "wait RUN_ID", Short: "Wait for terminal status and print the final run", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		run, err := s.waitRun(ctx, args[0])
		if err != nil {
			return err
		}
		if err = s.printRun(run, ""); err != nil {
			return err
		}
		return runExit(run)
	}}
	wait.Flags().DurationVar(&timeout, "timeout", 0, "Maximum observation time; zero waits until interrupted")
	root.AddCommand(list, get, watch, wait, s.runAction("cancel"), s.runAction("resume"), s.resolveCommand())
	return root
}
func (s *commandState) runAction(action string) *cobra.Command {
	var key string
	cmd := &cobra.Command{Use: action + " RUN_ID", Short: "Request engine action: " + action, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return s.mutation(cmd.Context(), "/runs/"+url.PathEscape(args[0])+"/"+action, map[string]any{}, key)
	}}
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Stable operation ID for this exact command")
	return cmd
}
func (s *commandState) resolveCommand() *cobra.Command {
	var outcome, evidence, file, key string
	var outputs []string
	cmd := &cobra.Command{Use: "resolve RUN_ID INSTANCE_ID", Short: "Record verified evidence for an uncertain execution", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if outcome != "succeeded" && outcome != "not_started" && outcome != "failed" {
			return fmt.Errorf("--outcome must be succeeded, not_started or failed")
		}
		if evidence == "" {
			return fmt.Errorf("--evidence is required")
		}
		values, err := s.object(file, outputs)
		if err != nil {
			return err
		}
		return s.mutation(cmd.Context(), "/runs/"+url.PathEscape(args[0])+"/instances/"+url.PathEscape(args[1])+"/resolve", map[string]any{"outcome": outcome, "evidence": evidence, "outputs": values}, key)
	}}
	cmd.Flags().StringVar(&outcome, "outcome", "", "Verified outcome: succeeded, not_started, failed")
	cmd.Flags().StringVar(&evidence, "evidence", "", "Evidence supporting the outcome")
	cmd.Flags().StringVar(&file, "outputs", "", "Verified JSON output object file; - reads stdin")
	cmd.Flags().StringArrayVar(&outputs, "output", nil, "Verified output NAME=JSON (repeatable)")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Stable operation ID")
	return cmd
}
func (s *commandState) getRun(ctx context.Context, id string) (protocol.Run, error) {
	var result struct {
		Run protocol.Run `json:"run"`
	}
	err := s.client().Get(ctx, "/runs/"+url.PathEscape(id), &result)
	return result.Run, err
}
func (s *commandState) waitRun(ctx context.Context, id string) (protocol.Run, error) {
	for {
		run, err := s.getRun(ctx, id)
		if err != nil {
			return run, err
		}
		if protocol.Terminal(run.Status) {
			return run, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return run, ctx.Err()
		case <-timer.C:
		}
	}
}
func runExit(run protocol.Run) error {
	if run.Status == "failed" || run.Status == "cancelled" {
		return &ExitError{Code: 3, Message: "run " + run.ID + " " + run.Status}
	}
	return nil
}
func (s *commandState) printRun(run protocol.Run, operation string) error {
	if s.json {
		result := map[string]any{"run": run}
		if operation != "" {
			result["operationId"] = operation
		}
		return s.printJSON(result)
	}
	if _, err := fmt.Fprintf(s.options.Out, "Run: %s\nStatus: %s\nProfile: %s\n", run.ID, run.Status, run.Profile); err != nil {
		return err
	}
	if operation != "" {
		if _, err := fmt.Fprintln(s.options.Out, "Operation:", operation); err != nil {
			return err
		}
	}
	if len(run.Instances) > 0 {
		rows := [][]string{}
		for _, item := range run.Instances {
			rows = append(rows, []string{item.ID, item.NodeID, item.Status, item.AttemptID})
		}
		if err := s.table([]string{"INSTANCE", "NODE", "STATUS", "ATTEMPT"}, rows); err != nil {
			return err
		}
	}
	if len(run.Outputs) > 0 {
		if err := s.printJSON(responseObject(run.Outputs)); err != nil {
			return err
		}
	}
	if len(run.Artifacts) > 0 {
		if err := s.printJSON(map[string]any{"artifacts": run.Artifacts}); err != nil {
			return err
		}
	}
	return s.diagnostics(run.Diagnostics)
}
func (s *commandState) mutation(ctx context.Context, route string, payload any, key string) error {
	id := operationKey(key)
	result := map[string]any{}
	if err := s.client().Command(ctx, route, payload, id, &result); err != nil {
		return err
	}
	result["operationId"] = id
	return s.printJSON(result)
}
func fetchPages[T any](ctx context.Context, s *commandState, route, cursor string, all bool) (protocol.Page[T], error) {
	result := protocol.Page[T]{Items: []T{}}
	seen := map[string]bool{}
	for {
		endpoint := route
		if cursor != "" {
			endpoint += "?cursor=" + url.QueryEscape(cursor)
		}
		var page protocol.Page[T]
		if err := s.client().Get(ctx, endpoint, &page); err != nil {
			return result, err
		}
		result.Items = append(result.Items, page.Items...)
		result.NextCursor = page.NextCursor
		if !all || page.NextCursor == nil {
			return result, nil
		}
		cursor = *page.NextCursor
		if cursor == "" || seen[cursor] {
			return result, fmt.Errorf("engine returned a nonadvancing pagination cursor")
		}
		seen[cursor] = true
	}
}
func pageFlags(cmd *cobra.Command, cursor *string, all *bool) {
	cmd.Flags().StringVar(cursor, "cursor", "", "Opaque pagination cursor")
	cmd.Flags().BoolVar(all, "all", false, "Read all pages")
}
func (s *commandState) nextCursor(cursor *string) error {
	if cursor != nil {
		_, err := fmt.Fprintln(s.options.Out, "Next cursor:", *cursor)
		return err
	}
	return nil
}
