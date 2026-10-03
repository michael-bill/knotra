package contract

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"cel.dev/cel-go/cel"
)

func TestCELMapIterationIsDeterministic(t *testing.T) {
	object, _ := JSONValue(map[string]any{"z": 1, "a": 2, "m": 3})
	nested, _ := JSONValue(map[string]any{"b": map[string]any{"z": 0, "a": 1}, "a": map[string]any{"y": 0, "b": 1}})
	scope := Scope{Args: Values{"object": object, "nested": nested}}

	for _, test := range []struct{ source, want string }{
		{`{"z": 1, "a": 2, "m": 3}.map(k, k)`, `["a","m","z"]`},
		{`args.object.map(k, k)`, `["a","m","z"]`},
		{`args.nested.map(k, args.nested[k].map(j, j))`, `[["b","y"],["a","z"]]`},
		{`[{"z": 1, "a": 2}, {"x": 1, "b": 2}].map(m, m.map(k, k))`, `[["a","z"],["b","x"]]`},
		{`(true ? args.object : {}).map(k, k)`, `["a","m","z"]`},
		{`[args.object][0].map(k, k)`, `["a","m","z"]`},
		{`{"outer": {"z": 1, "a": 2}}.outer.map(k, k)`, `["a","z"]`},
		{`[{"z": 1, "a": 2}][0].map(k, k)`, `["a","z"]`},
		{`args.object.filter(k, true).map(k, k)`, `["a","m","z"]`},
		{`[3, 1, 2].filter(n, n > 0).map(n, n)`, `[3,1,2]`},
		{`{"😀": 1, "ж": 1, "é": 1, "z": 1}.map(k, k)`, `["z","é","ж","😀"]`},
		{`{-10: 1, 2: 1, 1: 1}.map(k, k)`, `[-10,1,2]`},
		{`{true: 1, false: 1}.map(k, k)`, `[false,true]`},
		{
			`{"z": 0, 2: 0, false: 0, "a": 0, true: 0, -1: 0}.map(k, string(k))`,
			`["false","true","-1","2","a","z"]`,
		},
	} {
		t.Run(test.source, func(t *testing.T) {
			for i := 0; i < 128; i++ {
				value, present, err := EvalBinding(Binding{Expr: test.source}, scope)
				if err != nil || !present {
					t.Fatalf("evaluate map macro: %v", err)
				}
				if string(value.JSON) != test.want {
					t.Fatalf("map iteration is not stable: got %s, want %s", value.JSON, test.want)
				}
			}
		})
	}
}

func TestCELOrderedRangeRetainsStandardCost(t *testing.T) {
	var options []cel.EnvOption

	for _, name := range []string{"inputs", "args", "nodes", "state", "iteration", "body"} {
		options = append(options, cel.Variable(name, cel.DynType))
	}

	environment, err := cel.NewEnv(options...)
	if err != nil {
		t.Fatal(err)
	}
	activation := map[string]any{"args": map[string]any{"object": map[string]any{"z": int64(1), "a": int64(2), "m": int64(3)}}}

	for _, source := range []string{
		`{"z": 1, "a": 2, "m": 3}.map(k, k)`,
		`args.object.map(k, k)`,
		`(true ? args.object : {}).map(k, k)`,
		`[args.object][0].map(k, k)`,
		`{"outer": {"z": 1, "a": 2}}.outer.map(k, k)`,
		`[{"z": 1, "a": 2}][0].map(k, k)`,
		`args.object.filter(k, true).map(k, k)`,
		`[{"z": 1, "a": 2}].map(m, m.map(k, k))`,
		`args.object.all(k, args.object[k] > 0)`,
		`args.object.exists(k, false)`,
		`args.object.exists_one(k, args.object[k] == 1)`,
	} {
		t.Run(source, func(t *testing.T) {
			ast, issues := environment.Compile(source)
			if issues.Err() != nil {
				t.Fatal(issues.Err())
			}
			standard, err := environment.Program(ast, cel.CostLimit(100000))
			if err != nil {
				t.Fatal(err)
			}
			ordered, err := expression(source)
			if err != nil {
				t.Fatal(err)
			}
			_, baseline, err := standard.Eval(activation)
			if err != nil {
				t.Fatal(err)
			}
			_, actual, err := ordered.program.Eval(activation)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(baseline.ActualCost(), actual.ActualCost()) {
				t.Fatalf("standard cost changed: %d -> %d", *baseline.ActualCost(), *actual.ActualCost())
			}
		})
	}
}

func TestCELCachedMapProgramConcurrentEvaluation(t *testing.T) {
	value, _ := JSONValue(map[string]any{"z": 1, "a": 2, "m": 3})
	before := string(value.JSON)
	scope := Scope{Args: Values{"object": value}}
	var workers sync.WaitGroup

	for range 16 {
		workers.Go(func() {
			for range 64 {
				got, present, err := EvalBinding(Binding{Expr: `args.object.map(k, k)`}, scope)
				if err != nil || !present || string(got.JSON) != `["a","m","z"]` {
					t.Errorf("concurrent deterministic evaluation: %s %v", got.JSON, err)
					return
				}
			}
		})
	}

	workers.Wait()
	if string(scope.Args["object"].JSON) != before {
		t.Fatal("expression mutated its input")
	}
}

func TestCELMapIterationRetainsCostLimit(t *testing.T) {
	object := map[string]any{}

	for i := range 150 {
		object[fmt.Sprintf("key_%03d", i)] = i
	}

	value, err := JSONValue(object)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = EvalBinding(Binding{Expr: `args.object.map(k, args.object.map(j, k + j))`}, Scope{Args: Values{"object": value}})
	if err == nil || !strings.Contains(err.Error(), "cost limit") {
		t.Fatalf("nested map computation did not enforce standard cost limit: %v", err)
	}
}

func TestCELRejectsInvalidDynamicMapKeys(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
	}{
		{"double", 1.5},
		{"null", nil},
		{"list", []any{1}},
		{"map", map[string]any{"field": 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := JSONValue(test.value)
			if err != nil {
				t.Fatal(err)
			}

			for _, source := range []string{
				`{args.key: 1}.map(k, true)`,
				`{args.key: 1, "valid": 2}.map(k, true)`,
				`{args.key: 1}.size()`,
				`{"outer": {args.key: 1}}.outer.size()`,
			} {
				_, _, err := EvalBinding(Binding{Expr: source}, Scope{Args: Values{"key": value}})
				if err == nil || !strings.Contains(err.Error(), "CEL map keys must be") {
					t.Fatalf("invalid dynamic key accepted or failed unexpectedly for %s: %v", source, err)
				}
			}
		})
	}
}
