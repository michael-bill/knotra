package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestContractFixtures(t *testing.T) {
	base := "../../contracts/v1/fixtures"
	raw, err := os.ReadFile(filepath.Join(base, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Cases []struct {
			ID, Document, EngineProfile, PackageRoot string
			Expected                                 struct{ Parse, Structural, Semantic string }
		}
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}

	for _, test := range manifest.Cases {
		t.Run(test.ID, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(base, test.Document))
			if err != nil {
				t.Fatal(err)
			}
			value, err := ParseDocument(data)
			if test.Expected.Parse == "reject" {
				if err == nil {
					t.Fatal("expected parse rejection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateStructure(value)
			if test.Expected.Structural == "reject" {
				if err == nil {
					t.Fatal("expected structural rejection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var diags []Diagnostic
			if testObject(t, value)["kind"] == "EngineProfile" {
				_, diags = ParseProfile(data)
			} else {
				var profile *Profile
				if test.EngineProfile != "" {
					b, err := os.ReadFile(filepath.Join(base, test.EngineProfile))
					if err != nil {
						t.Fatal(err)
					}
					p, d := ParseProfile(b)
					if HasErrors(d) {
						t.Fatal(d)
					}
					profile = &p
				}
				var pkg Package
				if test.PackageRoot != "" {
					root := filepath.Join(base, test.PackageRoot)
					entry, err := filepath.Rel(root, filepath.Join(base, test.Document))
					if err != nil {
						t.Fatal(err)
					}
					pkg, err = LoadPackageRoot(root, filepath.ToSlash(entry))
					if err != nil {
						t.Fatal(err)
					}
				} else {
					pkg = Package{
						Entrypoint: filepath.Base(test.Document),
						Source:     string(data),
						Files:      []File{{Path: filepath.Base(test.Document), Content: data}},
					}
				}
				_, diags = Compile(pkg, profile)
			}
			if test.Expected.Semantic == "reject" {
				if !HasErrors(diags) {
					t.Fatal("expected semantic rejection")
				}
			} else if HasErrors(diags) {
				t.Fatalf("unexpected diagnostics: %+v", diags)
			}
		})
	}
}

func TestStrictYAML(t *testing.T) {
	tests := []string{
		"a: 1\na: 2\n",
		"a: {b: 1, b: 2}",
		"a: &x 1\nb: *x",
		"a: !!str hello",
		"a: {<<: {b: 1}}",
		"1: a",
		"a: 0x10",
		"a: 0o10",
		"a: .nan",
		"a: +12",
		"a: 1_000",
		"a: 9223372036854775808",
		"a: 1e400",
		"a: 1e-400",
		"a: 1\n---\n",
		"%YAML 1.1\n---\na: 1",
		"a: [" + strings.Repeat("[", 64) + "0" + strings.Repeat("]", 64) + "]",
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if _, err := ParseDocument([]byte(source)); err == nil {
				t.Fatal("accepted invalid YAML")
			}
		})
	}

	v, err := ParseDocument([]byte("%YAML 1.2\n---\na: yes\nb: on\nc: 2026-10-03\nd: 1.0\ne: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := testObject(t, v)
	if m["a"] != "yes" || m["b"] != "on" || m["c"] != "2026-10-03" {
		t.Fatal(m)
	}
	if _, ok := m["d"].(float64); !ok {
		t.Fatal("lost double type")
	}
	if _, ok := m["e"].(int64); !ok {
		t.Fatal("lost int type")
	}
}

func TestStrictJSON(t *testing.T) {
	for _, source := range []string{
		`{"a":1,"a":2}`,
		`{"x":{"a":1,"a":2}}`,
		`"\ud800"`,
		`"\udc00"`,
		`"\ud800\u0000"`,
		`9223372036854775808`,
		`1e400`,
		`1e-400`,
		`{} {}`,
		`[NaN]`,
	} {
		if _, err := DecodeJSON([]byte(source)); err == nil {
			t.Fatalf("accepted invalid JSON %s", source)
		}
	}

	for _, source := range []string{`"\ud83d\ude00"`, `"\\ud800"`, `0e-400`, `-9223372036854775808`, `1.0`, `1`} {
		if _, err := DecodeJSON([]byte(source)); err != nil {
			t.Fatalf("valid JSON %s: %v", source, err)
		}
	}

	if _, err := DecodeJSON([]byte{'"', 0xff, '"'}); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
}

func val(t *testing.T, v any) Value {
	t.Helper()
	out, err := JSONValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBindingSemantics(t *testing.T) {
	s := Scope{
		Inputs: Values{"object": val(t, map[string]any{"a/b": map[string]any{"~x": nil}})},
		Args:   Values{"number": val(t, int64(3))},
		Nodes:  map[string]Values{"ready": {"value": val(t, "fallback")}},
	}
	out, ok, err := EvalBinding(
		Binding{Coalesce: []Binding{
			{From: "nodes.skipped.outputs.value"},
			{From: "inputs.object", Path: "/a~1b/~0x"},
			{From: "nodes.ready.outputs.value"},
		}},
		s,
	)
	if err != nil || !ok || string(out.JSON) != "null" {
		t.Fatalf("null coalesce: %s %v %v", out.JSON, ok, err)
	}
	if _, _, err := EvalBinding(Binding{From: "inputs.object", Path: "/missing/~3"}, s); err == nil {
		t.Fatal("invalid pointer escape hidden by missing parent")
	}
	b, present, err := EvalBool(`has(args.absent) ? false : args.number == 3`, s)
	if err != nil || !present || !b {
		t.Fatalf("optional args: %v %v %v", b, present, err)
	}
	_, present, err = EvalBool(`true || has(nodes.skipped.outputs.value)`, s)
	if err != nil || present {
		t.Fatalf("graph absence must dominate short circuit: %v %v", present, err)
	}

	for _, expr := range []string{`[1,2,3].map(x, x * 2)`, `{"a": args.number}`, `1.0 + 2.0`, `inputs.object["a/b"]["~x"]`} {
		v, ok, err := EvalBinding(Binding{Expr: expr}, s)
		if err != nil || !ok {
			t.Fatalf("expression %s: %v", expr, err)
		}
		if expr == `[1,2,3].map(x, x * 2)` && string(v.JSON) != "[2,4,6]" {
			t.Fatal(string(v.JSON))
		}
	}

	v, _, err := EvalBinding(Binding{Expr: "1.0 + 2.0"}, s)
	if err != nil || string(v.JSON) != "3.0" {
		t.Fatal("double type lost", string(v.JSON), err)
	}
}

func TestRestrictedCEL(t *testing.T) {
	for _, source := range []string{
		`nodes[args.name].outputs.x`,
		`inputs`,
		`nodes.x.outputs[args.port]`,
		`bytes("x")`,
		`timestamp("2020-01-01T00:00:00Z")`,
		`1u`,
		`{1: "a"}`,
		`[1].map(inputs, inputs)`,
		`"a".replace("a", "b")`,
	} {
		_, _, err := EvalBinding(Binding{Expr: source}, Scope{Args: Values{"name": val(t, "x"), "port": val(t, "x")}})
		if err == nil {
			t.Errorf("accepted unsupported expression %s", source)
		}
	}

	if _, _, err := EvalBool(`42`, Scope{}); err == nil {
		t.Fatal("numeric condition accepted")
	}
	if _, err := expression(strings.Repeat(" ", 8193) + "1"); err == nil {
		t.Fatal("oversized expression accepted")
	}
}

func TestPortValidation(t *testing.T) {
	ports := map[string]Port{
		"x":        {Schema: json.RawMessage(`{"type":"integer"}`), Default: json.RawMessage(`4`)},
		"nullable": {Schema: json.RawMessage(`{"type":["null","string"]}`), Default: json.RawMessage(`"default"`)},
	}
	got, err := ValidatePorts(ports, Values{"nullable": val(t, nil)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got["x"].JSON) != "4" || string(got["nullable"].JSON) != "null" {
		t.Fatal(got)
	}
	if _, err := ValidatePorts(ports, Values{}, false); err == nil {
		t.Fatal("missing outputs accepted")
	}
	if _, err := ValidatePorts(ports, Values{"extra": val(t, 1)}, true); err == nil {
		t.Fatal("unknown port accepted")
	}
}

func TestDataSchemaProfile(t *testing.T) {
	for _, raw := range []string{
		`{"format":"date"}`,
		`{"$ref":"https://example.org/schema"}`,
		`{"$ref":"#"}`,
		`{"$defs":{"x":{"$ref":"#/$defs/x"}},"$ref":"#/$defs/x"}`,
		`{"pattern":"(?=a)a"}`,
		`{"pattern":"\\p{L}"}`,
		`{"pattern":"["}`,
		`{"required":["x","x"]}`,
	} {
		if _, err := compileDataSchema(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid schema %s", raw)
		}
	}

	raw := json.RawMessage(`{"$defs":{"word":{"type":"string","pattern":"^[a-z]+$"}},"$ref":"#/$defs/word"}`)
	if err := ValidateValue(Port{Schema: raw}, val(t, "word")); err != nil {
		t.Fatal(err)
	}
	composite := PortObjectSchema(map[string]Port{"answer": {Schema: raw}})
	schema, err := compileDataSchema(composite)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(map[string]any{"answer": "word"}); err != nil {
		t.Fatal(err)
	}
	wrapped := wrapSchema(raw, "items", map[string]any{"type": "array"})
	if err := ValidateValue(Port{Schema: wrapped}, val(t, []any{"word"})); err != nil {
		t.Fatal(err)
	}
}

func TestPackageConfinement(t *testing.T) {
	root := t.TempDir()
	base := []byte("apiVersion: knotra/v1\nkind: Pipeline\nmetadata: {name: test}\nspec:\n  files: [payload.txt]\n  nodes:\n    ask:\n      type: human\n      human: {prompt: {text: Ask}}\n      outputs: {answer: {schema: {type: string}}}\n  outputs:\n    answer: {schema: {type: string}, bind: {from: nodes.ask.outputs.answer}}\n")
	if err := os.WriteFile(filepath.Join(root, "pipeline.yaml"), base, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPackage(filepath.Join(root, "pipeline.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := packageFiles(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Files[0], p.Files[1] = p.Files[1], p.Files[0]
	_, other, err := packageFiles(p)
	if err != nil || digest != other {
		t.Fatal("manifest digest depends on input ordering")
	}
	if err := os.Remove(filepath.Join(root, "payload.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pipeline.yaml", filepath.Join(root, "payload.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPackage(filepath.Join(root, "pipeline.yaml")); err == nil {
		t.Fatal("symlink accepted")
	}

	for _, path := range []string{"../escape", "a//b", "a/./b", "C:/file", "a\\b", "a\nb", "cafe\u0301.txt"} {
		if err := validPath(path); err == nil {
			t.Errorf("invalid path accepted %q", path)
		}
	}

	collision := Package{Entrypoint: "Main.yaml", Files: []File{{Path: "Main.yaml"}, {Path: "main.yaml"}}}
	if _, _, err := packageFiles(collision); err == nil {
		t.Fatal("case collision accepted")
	}
}

func TestDuration(t *testing.T) {
	for _, s := range []string{"0s", "01s", "1h30m", "8761h", "99999999999999999999999s", "1s\n"} {
		if _, err := Duration(s); err == nil {
			t.Errorf("invalid duration accepted %q", s)
		}
	}

	if d, err := Duration("8760h"); err != nil || d.Hours() != 8760 {
		t.Fatal(d, err)
	}
}

func TestJSONRoundTripTypes(t *testing.T) {
	original := map[string]any{"i": int64(1), "d": float64(1), "a": []any{int64(-1), float64(-1)}}
	value, err := JSONValue(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeJSON(value.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("types lost: %#v -> %#v", original, decoded)
	}
}

func TestSemanticPreparation(t *testing.T) {
	source, err := os.ReadFile("../../contracts/v1/fixtures/positive/human/pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	base, err := ParseDocument(source)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		edit func(*testing.T, map[string]any)
	}{
		{"nonboolean condition", func(t *testing.T, m map[string]any) {
			nodes := testObject(t, testObject(t, m["spec"])["nodes"])

			for _, n := range nodes {
				testObject(t, n)["when"] = "1"
			}
		}},
		{"unknown graph source", func(t *testing.T, m map[string]any) {
			nodes := testObject(t, testObject(t, m["spec"])["nodes"])

			for _, n := range nodes {
				testObject(t, n)["when"] = "false && nodes.absent.outputs.x"
			}
		}},
		{"whole namespace", func(t *testing.T, m map[string]any) {
			nodes := testObject(t, testObject(t, m["spec"])["nodes"])

			for _, n := range nodes {
				testObject(t, n)["when"] = "size(inputs) == 0"
			}
		}},
		{"control retry", func(t *testing.T, m map[string]any) {
			testObject(t, m["spec"])["defaults"] = map[string]any{"execution": map[string]any{"retry": map[string]any{"maxAttempts": int64(2)}}}
		}},
		{"unknown default grant", func(t *testing.T, m map[string]any) {
			testObject(t, m["spec"])["defaults"] = map[string]any{"tools": map[string]any{"mcp": map[string]any{"absent": []any{"tool"}}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			cloned, err := DecodeJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			test.edit(t, testObject(t, cloned))
			raw, _ = json.Marshal(cloned)
			_, diags := Compile(Package{Entrypoint: "pipeline.yaml", Files: []File{{Path: "pipeline.yaml", Content: raw}}}, nil)
			if !HasErrors(diags) {
				t.Fatal("semantic error accepted")
			}
			if diags[0].Line == 0 || diags[0].Column == 0 {
				t.Fatal("source location missing", diags)
			}
		})
	}
}

func TestCELCostLimit(t *testing.T) {
	list := make([]any, 1000)

	for i := range list {
		list[i] = int64(i)
	}

	_, _, err := EvalBinding(
		Binding{Expr: `args.items.all(x, args.items.all(y, x + y >= 0))`},
		Scope{Args: Values{"items": val(t, list)}},
	)
	if err == nil || !strings.Contains(err.Error(), "cost limit") {
		t.Fatalf("expected cost limit error, got %v", err)
	}
}

func TestCELStaticDependencies(t *testing.T) {
	expression, err := expression(`false ? nodes.early.outputs.value : nodes.late.outputs.value`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expression.refs, []string{"nodes.early.outputs.value", "nodes.late.outputs.value"}) {
		t.Fatal(expression.refs)
	}
}

func TestProfilesAndOfflineDelegation(t *testing.T) {
	data, err := os.ReadFile("../../contracts/v1/fixtures/positive/engine-profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, diags := ParseProfile(data)
	if HasErrors(diags) {
		t.Fatal(diags)
	}
	bad := p
	bad.Spec.MCP = map[string]MCPConnection{"bad": {
		Transport:    "streamable_http",
		URL:          "https://user:pass@example.org/#x",
		Headers:      map[string]Credential{"HOST": {}},
		AllowedTools: []string{"search"},
		ToolPolicies: map[string]ToolPolicy{"write": {Effect: "write"}},
	}}
	if diags := validateProfile(bad); len(diags) < 3 {
		t.Fatalf("missed profile violations: %v", diags)
	}
	pkg, err := LoadPackage("../../contracts/v1/fixtures/negative/subpipeline-permission/pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, diags := Compile(pkg, nil); !HasErrors(diags) {
		t.Fatal("offline delegation accepted alias instead of canonical ID")
	}
}

func TestPointerErrorsAndMissing(t *testing.T) {
	for _, source := range []string{"null", "42", "true", `"string"`} {
		v, _ := DecodeJSON([]byte(source))
		if _, _, err := pointer(v, "/x"); err == nil {
			t.Fatalf("scalar traversal accepted: %s", source)
		}
	}

	for _, key := range []string{"-", "-1", "01", "+1", "x", ""} {
		if _, _, err := pointer([]any{}, "/"+key); err == nil {
			t.Fatalf("invalid array index accepted %q", key)
		}
	}

	if _, present, err := pointer([]any{}, "/100000000000000000000000000000"); err != nil || present {
		t.Fatal("valid out-of-range index should be missing", err)
	}
}

func TestSchemaRelocationPreservesValues(t *testing.T) {
	original := json.RawMessage(`{"$defs":{"number":{"const":9223372036854775807}},"type":"object","properties":{"const":{"$ref":"#/$defs/number"},"literal":{"const":{"$ref":"#not-a-schema-ref"}}},"required":["const","literal"]}`)
	combined := PortObjectSchema(map[string]Port{"out": {Schema: original}})
	value := val(
		t,
		map[string]any{"out": map[string]any{
			"const":   int64(9223372036854775807),
			"literal": map[string]any{"$ref": "#not-a-schema-ref"},
		}},
	)
	if err := ValidateValue(Port{Schema: combined}, value); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidUnicodeNativeValue(t *testing.T) {
	for _, input := range []any{
		string([]byte{0xff}),
		map[string]any{string([]byte{0xff}): "value"},
		[]string{string([]byte{0xff})},
	} {
		if _, err := JSONValue(input); err == nil {
			t.Fatal("invalid UTF-8 silently repaired")
		}
	}

	var circular any
	circular = &circular
	if _, err := JSONValue(circular); err == nil {
		t.Fatal("circular native value accepted")
	}
}

func testObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected JSON object, got %T", value)
	}
	return object
}

func TestPortDependenciesIncludeCoalesceAndCheckedCELReferences(t *testing.T) {
	deps, err := PortDependencies(map[string]Port{
		"fallback": {Bind: &Binding{Coalesce: []Binding{{From: "nodes.a.outputs.result"}, {Expr: `false ? nodes.b.outputs.result : nodes.c.outputs.result`}}}},
		"literal":  {Bind: &Binding{Value: json.RawMessage(`"nodes.fake.outputs.result"`)}},
		"input":    {Bind: &Binding{From: "inputs.value"}},
	})
	if err != nil || !reflect.DeepEqual(deps, []string{"a", "b", "c"}) {
		t.Fatalf("dependencies=%v error=%v", deps, err)
	}
	if _, err := PortDependencies(map[string]Port{"bad": {Bind: &Binding{Expr: "nodes[inputs.dynamic]"}}}); err == nil {
		t.Fatal("dynamic node access bypassed checked dependencies")
	}
}
