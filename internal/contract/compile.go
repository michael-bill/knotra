package contract

import (
	"encoding/json"
	"fmt"
	"mime"
	"path"
	"strings"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"google.golang.org/protobuf/encoding/protojson"
)

const CompilerVersion = "knotra-go/v1"
const CELVersion = "cel-go/v0.32.0"

// HasErrors distinguishes deferred admission warnings from rejected documents.
func HasErrors(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == "error" {
			return true
		}
	}
	return false
}

type compiler struct {
	plan               *Plan
	files              map[string][]byte
	admitted           map[string]bool
	visiting, finished map[string]bool
	diags              []Diagnostic
	count              int
	positions          map[string]map[string]sourcePosition
}

// Compile performs deterministic offline compilation, optionally checking the
// trusted profile. External capability discovery is a separate admission step.
func Compile(pkg Package, profile *Profile) (*Plan, []Diagnostic) {
	files, digest, err := packageFiles(pkg)
	if err != nil {
		return nil, []Diagnostic{diagnostic("PACKAGE_INVALID", "package", pkg.Entrypoint, "", err)}
	}
	c := &compiler{plan: &Plan{Version: "knotra/v1", Digest: digest, Package: pkg, Root: pkg.Entrypoint, Pipelines: map[string]*Pipeline{}, Expressions: map[string]json.RawMessage{}, CompilerVersion: CompilerVersion, CELVersion: CELVersion}, files: files, admitted: map[string]bool{pkg.Entrypoint: true}, visiting: map[string]bool{}, finished: map[string]bool{}, positions: map[string]map[string]sourcePosition{}}
	if profile != nil {
		raw, marshalErr := json.Marshal(profile)
		if marshalErr != nil {
			c.add("PROFILE_INVALID", "structural", "", "", marshalErr)
		} else {
			// Admission pins resource metadata in this snapshot. A shallow copy
			// would mutate the shared server profile and race concurrent runs.
			c.add("PROFILE_INVALID", "structural", "", "", json.Unmarshal(raw, &c.plan.Profile))
			value, parseErr := DecodeJSON(raw)
			if parseErr != nil {
				c.add("PROFILE_INVALID", "structural", "", "", parseErr)
			} else {
				c.add("PROFILE_INVALID", "structural", "", "", ValidateStructure(value))
			}
		}
		c.diags = append(c.diags, validateProfile(*profile)...)
	}
	c.pipeline(pkg.Entrypoint, 0)
	if !HasErrors(c.diags) && c.planDepth() > 32 {
		c.add("GRAPH_DEPTH", "semantic", pkg.Entrypoint, "/spec", fmt.Errorf("combined graph/import depth exceeds 32"))
	}
	for _, name := range sortedKeys(files) {
		if !c.admitted[name] {
			c.add("PACKAGE_UNDECLARED", "package", name, "", fmt.Errorf("file not included by any spec.files"))
		}
	}
	if profile != nil && !HasErrors(c.diags) {
		c.admit(c.plan.Root, nil, profile.Spec.Limits, 0)
	} else if profile == nil && !HasErrors(c.diags) {
		c.offlineDelegation(c.plan.Root, nil, Limits{}, 0)
	}
	if HasErrors(c.diags) {
		sortDiagnostics(c.diags)
		return nil, c.diags
	}
	if profile == nil {
		c.diags = append(c.diags, Diagnostic{Severity: "warning", Code: "ADMISSION_PENDING", Phase: "admission", File: pkg.Entrypoint, Message: "External capabilities, profile permissions, credentials, images and tools require engine admission."})
	}
	return c.plan, c.diags
}

func (c *compiler) planDepth() int {
	memo := map[string]int{}
	var pipeline func(string) int
	var graph func(Graph) int
	graph = func(g Graph) int {
		maximum := 0
		for _, n := range g.Nodes {
			depth := 0
			if n.Foreach != nil {
				depth = 1 + graph(n.Foreach.Body)
			}
			if n.Loop != nil {
				depth = 1 + graph(n.Loop.Body)
			}
			if n.Pipeline != nil {
				depth = 1 + pipeline(n.Pipeline.File)
			}
			if depth > maximum {
				maximum = depth
			}
		}
		return maximum
	}
	pipeline = func(file string) int {
		if d, ok := memo[file]; ok {
			return d
		}
		p := c.plan.Pipelines[file]
		if p == nil {
			return 0
		}
		d := graph(p.Spec.Graph)
		memo[file] = d
		return d
	}
	return pipeline(c.plan.Root)
}

func (c *compiler) add(code, phase, file, p string, err error) {
	if err != nil {
		d := diagnostic(code, phase, file, p, err)
		if source, ok := c.files[file]; ok {
			positions, exists := c.positions[file]
			if !exists {
				positions = sourcePositions(source)
				c.positions[file] = positions
			}
			d = locateWithPositions(d, positions)
		}
		c.diags = append(c.diags, d)
	}
}
func (c *compiler) semantic(file, p string, err error) {
	c.add("SEMANTIC_INVALID", "semantic", file, p, err)
}

func (c *compiler) pipeline(file string, depth int) *Pipeline {
	if c.visiting[file] {
		c.add("IMPORT_CYCLE", "package", file, "", fmt.Errorf("cyclic pipeline import"))
		return nil
	}
	if depth > 32 {
		c.add("IMPORT_DEPTH", "package", file, "", fmt.Errorf("import depth exceeds 32"))
		return nil
	}
	if c.finished[file] {
		return c.plan.Pipelines[file]
	}
	data, exists := c.files[file]
	if !exists {
		c.add("PACKAGE_MISSING", "package", file, "", fmt.Errorf("file not included"))
		return nil
	}
	p, diags := parsePipeline(data, file)
	c.diags = append(c.diags, diags...)
	if HasErrors(diags) {
		return nil
	}
	c.visiting[file] = true
	c.plan.Pipelines[file] = &p
	for _, f := range p.Spec.Files {
		c.admitted[f] = true
		if _, ok := c.files[f]; !ok {
			c.add("PACKAGE_MISSING", "package", file, "/spec/files", fmt.Errorf("missing file %q", f))
		}
	}
	for _, edge := range importEdges(p.Spec.Graph, 0) {
		child := edge.file
		if !c.admitted[child] {
			c.add("PACKAGE_UNDECLARED", "package", file, "/spec/nodes", fmt.Errorf("import %q not declared", child))
			continue
		}
		c.pipeline(child, depth+edge.depth+1)
	}
	registry := map[string]json.RawMessage{}
	for _, name := range sortedKeys(p.Spec.Schemas) {
		raw := p.Spec.Schemas[name]
		var source map[string]json.RawMessage
		if json.Unmarshal(raw, &source) == nil && source["file"] != nil {
			var name string
			_ = json.Unmarshal(source["file"], &name)
			raw = c.schemaFile(file, name)
		}
		if _, err := compileDataSchema(raw); err != nil {
			c.semantic(file, "/spec/schemas/"+name, err)
		}
		registry[name] = raw
	}
	p.Spec.Schemas = registry
	for name, m := range p.Spec.MCP {
		if m.Session == "" {
			m.Session = "node"
		}
		p.Spec.MCP[name] = m
	}
	c.checkDuration(file, "/spec/limits/timeout", p.Spec.Limits.Timeout)
	c.graph(file, &p.Spec, &p.Spec.Graph, "/spec", depth)
	delete(c.visiting, file)
	c.finished[file] = true
	return &p
}

func (c *compiler) schemaFile(owner, name string) json.RawMessage {
	data, ok := c.files[name]
	if !ok || !c.admitted[name] {
		c.add("SCHEMA_FILE", "package", owner, "", fmt.Errorf("schema file %q not admitted", name))
		return nil
	}
	var v any
	var err error
	switch strings.ToLower(path.Ext(name)) {
	case ".json":
		v, err = DecodeJSON(data)
	case ".yaml", ".yml":
		v, err = ParseDocument(data)
	default:
		err = fmt.Errorf("unsupported schema file extension")
	}
	if err != nil {
		c.add("SCHEMA_FILE", "package", owner, name, err)
		return nil
	}
	raw, err := marshalJSON(v)
	c.add("SCHEMA_FILE", "package", owner, name, err)
	return raw
}

func (c *compiler) ports(file string, spec *Spec, ports map[string]Port, prefix string) {
	for _, name := range sortedKeys(ports) {
		p := ports[name]
		where := prefix + "/" + name
		if p.SchemaRef != "" {
			schema, ok := spec.Schemas[p.SchemaRef]
			if !ok {
				c.semantic(file, where, fmt.Errorf("unknown schemaRef %q", p.SchemaRef))
			} else {
				p.Schema = schema
				p.SchemaRef = ""
			}
		}
		if p.Artifact == nil {
			if _, err := compileDataSchema(p.Schema); err != nil {
				c.semantic(file, where+"/schema", err)
			}
		} else {
			for _, mt := range p.Artifact.MediaTypes {
				actual, params, err := mime.ParseMediaType(mt)
				if err != nil || len(params) != 0 || actual != mt || strings.Contains(mt, "*") {
					c.semantic(file, where, fmt.Errorf("invalid exact MIME type %q", mt))
				}
			}
			if p.Collect != nil && !contains(p.Artifact.MediaTypes, p.Collect.MediaType) {
				c.semantic(file, where, fmt.Errorf("collect mediaType outside permitted types"))
			}
		}
		if len(p.Default) > 0 {
			c.semantic(file, where+"/default", ValidateValue(p, Value{JSON: p.Default}))
		}
		ports[name] = p
	}
}

type portScope struct {
	inputs, args, state, body map[string]Port
	nodes                     map[string]Node
	item                      *Port
	index                     bool
}

func (s portScope) resolve(ref string) (Port, error) {
	parts := strings.Split(ref, ".")
	if len(parts) < 2 {
		return Port{}, fmt.Errorf("invalid reference %q", ref)
	}
	var ports map[string]Port
	var name string
	switch parts[0] {
	case "inputs":
		ports = s.inputs
		name = parts[1]
	case "args":
		ports = s.args
		name = parts[1]
	case "state":
		ports = s.state
		name = parts[1]
	case "body":
		if len(parts) != 3 || parts[1] != "outputs" {
			return Port{}, fmt.Errorf("invalid body reference")
		}
		ports = s.body
		name = parts[2]
	case "nodes":
		if len(parts) != 4 || parts[2] != "outputs" {
			return Port{}, fmt.Errorf("invalid node reference")
		}
		n, ok := s.nodes[parts[1]]
		if !ok {
			return Port{}, fmt.Errorf("unknown node %q", parts[1])
		}
		ports = n.Outputs
		name = parts[3]
	case "iteration":
		if parts[1] == "index" && s.index {
			return Port{Schema: json.RawMessage(`{"type":"integer","minimum":0}`)}, nil
		}
		if parts[1] == "item" && s.item != nil {
			return *s.item, nil
		}
		return Port{}, fmt.Errorf("iteration field unavailable")
	default:
		return Port{}, fmt.Errorf("unknown namespace %q", parts[0])
	}
	p, ok := ports[name]
	if !ok {
		return Port{}, fmt.Errorf("reference %q is unavailable in this scope", ref)
	}
	return p, nil
}

func (c *compiler) binding(file, where string, b *Binding, target *Port, s portScope, deps map[string]bool) {
	if b == nil {
		return
	}
	if len(b.Coalesce) > 0 {
		for i := range b.Coalesce {
			c.binding(file, fmt.Sprintf("%s/coalesce/%d", where, i), &b.Coalesce[i], target, s, deps)
		}
		return
	}
	if b.From != "" {
		source, err := s.resolve(b.From)
		if err != nil {
			c.semantic(file, where, err)
			return
		}
		if strings.HasPrefix(b.From, "nodes.") {
			deps[strings.Split(b.From, ".")[1]] = true
		}
		if b.pathPresent && source.Artifact != nil {
			c.semantic(file, where, fmt.Errorf("artifact cannot use JSON Pointer"))
		}
		if b.Path != "" {
			if source.Artifact != nil {
				c.semantic(file, where, fmt.Errorf("artifact cannot use JSON Pointer"))
			}
			_, _, err := pointer(map[string]any{}, b.Path)
			c.semantic(file, where, err)
			source.Schema = json.RawMessage(`true`)
		}
		if target != nil {
			c.semantic(file, where, compatible(source, *target))
		}
		return
	}
	if b.Expr != "" {
		if target != nil && target.Artifact != nil {
			c.semantic(file, where, fmt.Errorf("artifact binding must be from/coalesce"))
		}
		c.expr(file, where, b.Expr, s, deps, false)
		return
	}
	if len(b.Value) > 0 && target != nil {
		c.semantic(file, where, ValidateValue(*target, Value{JSON: b.Value}))
	}
}

func (c *compiler) expr(file, where, source string, s portScope, deps map[string]bool, boolean bool) {
	x, err := expression(source)
	if err != nil {
		c.semantic(file, where, err)
		return
	}
	if boolean && !x.ast.OutputType().IsExactType(cel.BoolType) && !x.ast.OutputType().IsExactType(cel.DynType) {
		c.semantic(file, where, fmt.Errorf("condition must produce bool"))
	}
	for _, ref := range x.refs {
		p, err := s.resolve(ref)
		if err != nil {
			c.semantic(file, where, err)
		} else if p.Artifact != nil {
			c.semantic(file, where, fmt.Errorf("artifacts cannot be accessed by CEL"))
		}
		if strings.HasPrefix(ref, "nodes.") {
			deps[strings.Split(ref, ".")[1]] = true
		}
	}
	ast, err := cel.AstToCheckedExpr(x.ast)
	if err == nil {
		raw, err := protojson.Marshal(ast)
		if err == nil {
			c.plan.Expressions[source] = raw
		}
	}
}

func compatible(source, target Port) error {
	if (source.Artifact == nil) != (target.Artifact == nil) {
		return fmt.Errorf("JSON and artifact ports are incompatible")
	}
	if source.Artifact != nil {
		if source.Artifact.Collection != target.Artifact.Collection {
			return fmt.Errorf("artifact collection shape mismatch")
		}
		overlap := false
		for _, mt := range source.Artifact.MediaTypes {
			overlap = overlap || contains(target.Artifact.MediaTypes, mt)
		}
		if !overlap {
			return fmt.Errorf("artifact media types are disjoint")
		}
		return nil
	}
	if string(source.Schema) == "false" || string(target.Schema) == "false" {
		return fmt.Errorf("binding cannot satisfy a false schema")
	}
	for _, pair := range [][2]json.RawMessage{{source.Schema, target.Schema}, {target.Schema, source.Schema}} {
		var object map[string]json.RawMessage
		if json.Unmarshal(pair[0], &object) != nil {
			continue
		}
		var values []json.RawMessage
		if constant, ok := object["const"]; ok {
			values = []json.RawMessage{constant}
		} else if enumeration, ok := object["enum"]; ok {
			_ = json.Unmarshal(enumeration, &values)
		}
		if len(values) > 0 {
			schema, err := compileDataSchema(pair[1])
			if err == nil {
				overlap := false
				for _, raw := range values {
					value, err := DecodeJSON(raw)
					if err == nil && schema.Validate(value) == nil {
						overlap = true
						break
					}
				}
				if !overlap {
					return fmt.Errorf("JSON port value domains are disjoint")
				}
			}
		}
	}
	a, b := schemaTypes(source.Schema), schemaTypes(target.Schema)
	if len(a) > 0 && len(b) > 0 {
		for _, x := range a {
			for _, y := range b {
				if x == y || (x == "integer" && y == "number") || (x == "number" && y == "integer") {
					return nil
				}
			}
		}
		return fmt.Errorf("JSON port types are disjoint: %v and %v", a, b)
	}
	return nil
}
func schemaTypes(raw json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	switch t := m["type"].(type) {
	case string:
		return []string{t}
	case []any:
		out := []string{}
		for _, v := range t {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (c *compiler) graph(file string, spec *Spec, g *Graph, prefix string, depth int) {
	if depth > 32 {
		c.semantic(file, prefix, fmt.Errorf("graph depth exceeds 32"))
		return
	}
	c.ports(file, spec, g.Inputs, prefix+"/inputs")
	c.ports(file, spec, g.Outputs, prefix+"/outputs")
	for _, name := range sortedKeys(g.Nodes) {
		n := g.Nodes[name]
		where := prefix + "/nodes/" + name
		c.count++
		if c.count > 10000 {
			c.semantic(file, where, fmt.Errorf("static node count exceeds 10000"))
			return
		}
		c.ports(file, spec, n.Inputs, where+"/inputs")
		c.ports(file, spec, n.Outputs, where+"/outputs")
		n.Execution = c.execution(file, where, n.Type, spec.Defaults.Execution, n.Execution)
		switch n.Type {
		case "switch":
			routes := []string{n.Switch.Default}
			seen := map[string]bool{n.Switch.Default: true}
			for _, cs := range n.Switch.Cases {
				if seen[cs.Name] {
					c.semantic(file, where, fmt.Errorf("duplicate switch route %q", cs.Name))
				}
				seen[cs.Name] = true
				routes = append(routes, cs.Name)
			}
			raw, _ := json.Marshal(map[string]any{"type": "string", "enum": routes})
			n.Outputs = map[string]Port{"route": {Schema: raw}}
		case "foreach":
			c.graph(file, spec, &n.Foreach.Body, where+"/foreach/body", depth+1)
			n.Outputs = map[string]Port{}
			for _, key := range sortedKeys(n.Foreach.Body.Outputs) {
				p := n.Foreach.Body.Outputs[key]
				if !p.IsRequired() {
					c.semantic(file, where, fmt.Errorf("foreach body exports must be required"))
				}
				if p.Artifact != nil {
					if p.Artifact.Collection {
						c.semantic(file, where, fmt.Errorf("foreach cannot aggregate artifact collections"))
					}
					p.Artifact = &ArtifactPort{MediaTypes: p.Artifact.MediaTypes, Collection: true}
				} else {
					p.Schema = wrapSchema(p.Schema, "items", map[string]any{"type": "array"})
				}
				p.Bind = nil
				n.Outputs[key] = p
			}
		case "loop":
			c.ports(file, spec, n.Loop.State, where+"/loop/state")
			c.graph(file, spec, &n.Loop.Body, where+"/loop/body", depth+1)
			n.Outputs = map[string]Port{}
			for key, p := range n.Loop.Body.Outputs {
				if key == "iterations" || key == "termination" {
					c.semantic(file, where, fmt.Errorf("reserved loop output %q", key))
				}
				p.Bind = nil
				n.Outputs[key] = p
			}
			n.Outputs["iterations"] = Port{Schema: json.RawMessage(`{"type":"integer","minimum":1}`)}
			n.Outputs["termination"] = Port{Schema: json.RawMessage(`{"enum":["condition","limit"]}`)}
			if n.Loop.OnLimit == "" {
				n.Loop.OnLimit = "fail"
			}
		case "pipeline":
			if child := c.plan.Pipelines[n.Pipeline.File]; child != nil {
				n.Outputs = map[string]Port{}
				for key, p := range child.Spec.Outputs {
					p.Bind = nil
					n.Outputs[key] = p
				}
				for key := range n.Inputs {
					if p, ok := child.Spec.Inputs[key]; !ok {
						c.semantic(file, where, fmt.Errorf("unknown child input %q", key))
					} else {
						c.semantic(file, where, compatible(n.Inputs[key], p))
					}
				}
				for key, p := range child.Spec.Inputs {
					if _, ok := n.Inputs[key]; !ok && p.IsRequired() && len(p.Default) == 0 {
						c.semantic(file, where, fmt.Errorf("missing required child input %q", key))
					}
				}
			}
		case "agent":
			n.Tools = effectiveTools(spec.Defaults.Tools, n.Tools)
			c.text(file, where, &n.Agent.Prompt)
			c.text(file, where, &n.Agent.Instructions)
			if _, ok := spec.Models[n.Agent.Model]; !ok {
				c.semantic(file, where, fmt.Errorf("unknown model %q", n.Agent.Model))
			} else {
				model := spec.Models[n.Agent.Model]
				model.Requires = union(model.Requires, []string{"toolCalling"})
				spec.Models[n.Agent.Model] = model
			}
		case "llm":
			c.text(file, where, &n.LLM.Prompt)
			c.text(file, where, &n.LLM.Instructions)
			if _, ok := spec.Models[n.LLM.Model]; !ok {
				c.semantic(file, where, fmt.Errorf("unknown model %q", n.LLM.Model))
			}
		case "human":
			c.text(file, where, &n.Human.Prompt)
		case "tool":
			if _, ok := spec.MCP[n.Tool.Server]; !ok {
				c.semantic(file, where, fmt.Errorf("unknown MCP alias %q", n.Tool.Server))
			}
			if n.Tool.Response == "" {
				n.Tool.Response = "structured"
			}
		}
		if n.Type == "agent" || n.Type == "code" {
			if _, ok := spec.Sandboxes[n.Sandbox]; !ok {
				c.semantic(file, where, fmt.Errorf("unknown sandbox alias %q", n.Sandbox))
			}
		}
		mounts := []string{}
		for key, p := range n.Inputs {
			if p.Artifact != nil {
				if n.Type == "llm" {
					c.semantic(file, where, fmt.Errorf("llm only accepts JSON inputs"))
				}
				sandboxed := n.Type == "agent" || n.Type == "code"
				if sandboxed != (p.Mount != "") {
					c.semantic(file, where+"/inputs/"+key, fmt.Errorf("artifact mount required only on agent/code"))
				}
				if p.Mount != "" {
					for _, existing := range mounts {
						if existing == p.Mount || strings.HasPrefix(existing, p.Mount+"/") || strings.HasPrefix(p.Mount, existing+"/") {
							c.semantic(file, where, fmt.Errorf("overlapping input mounts"))
						}
					}
					mounts = append(mounts, p.Mount)
				}
			}
		}
		for env, v := range n.Env {
			if strings.HasPrefix(env, "KNOTRA_") {
				c.semantic(file, where, fmt.Errorf("reserved environment name %s", env))
			}
			if v.Secret != "" {
				if _, ok := spec.Secrets[v.Secret]; !ok {
					c.semantic(file, where, fmt.Errorf("unknown secret alias %s", v.Secret))
				}
			}
		}
		g.Nodes[name] = n
	}
	for _, name := range sortedKeys(g.Nodes) {
		n := g.Nodes[name]
		where := prefix + "/nodes/" + name
		deps := map[string]bool{}
		scope := portScope{inputs: g.Inputs, nodes: g.Nodes}
		for _, need := range n.Needs {
			if _, ok := g.Nodes[need]; !ok {
				c.semantic(file, where, fmt.Errorf("unknown dependency %q", need))
			}
			deps[need] = true
		}
		for key, p := range n.Inputs {
			c.binding(file, where+"/inputs/"+key, p.Bind, &p, scope, deps)
		}
		scope.args = n.Inputs
		if n.When != "" {
			c.expr(file, where+"/when", n.When, scope, deps, true)
		}
		local := portScope{args: n.Inputs}
		switch n.Type {
		case "tool":
			target := Port{Schema: json.RawMessage(`{"type":"object"}`)}
			c.binding(file, where+"/tool/arguments", &n.Tool.Arguments, &target, local, deps)
		case "switch":
			for i, cs := range n.Switch.Cases {
				c.expr(file, fmt.Sprintf("%s/switch/cases/%d/when", where, i), cs.When, local, deps, true)
			}
		case "foreach":
			over, ok := n.Inputs[n.Foreach.Over]
			if !ok {
				c.semantic(file, where, fmt.Errorf("unknown foreach input %q", n.Foreach.Over))
			}
			item := Port{Schema: json.RawMessage(`true`)}
			if over.Artifact != nil {
				if !over.Artifact.Collection {
					c.semantic(file, where, fmt.Errorf("foreach requires collection"))
				}
				item.Artifact = &ArtifactPort{MediaTypes: over.Artifact.MediaTypes}
				item.Schema = nil
			} else {
				types := schemaTypes(over.Schema)
				if len(types) > 0 && !contains(types, "array") {
					c.semantic(file, where, fmt.Errorf("foreach requires array input"))
				}
				var obj map[string]json.RawMessage
				if json.Unmarshal(over.Schema, &obj) == nil && obj["items"] != nil {
					item.Schema = obj["items"]
				}
			}
			local.item = &item
			local.index = true
			c.with(file, where, n.Foreach.With, n.Foreach.Body.Inputs, local, deps)
		case "loop":
			for key, p := range n.Loop.State {
				c.binding(file, where+"/loop/state/"+key+"/initial", p.Initial, &p, local, deps)
			}
			local.state = n.Loop.State
			local.index = true
			c.with(file, where, n.Loop.With, n.Loop.Body.Inputs, local, deps)
			local.body = n.Loop.Body.Outputs
			c.expr(file, where+"/loop/until", n.Loop.Until, local, deps, true)
			for key, p := range n.Loop.State {
				c.binding(file, where+"/loop/state/"+key+"/next", p.Next, &p, local, deps)
			}
		}
		n.Dependencies = sortedKeys(deps)
		g.Nodes[name] = n
	}
	for key, p := range g.Outputs {
		c.binding(file, prefix+"/outputs/"+key, p.Bind, &p, portScope{inputs: g.Inputs, nodes: g.Nodes}, map[string]bool{})
	}
	colors := map[string]int{}
	var visit func(string)
	visit = func(name string) {
		if colors[name] == 1 {
			c.add("GRAPH_CYCLE", "semantic", file, prefix, fmt.Errorf("dependency cycle through %q", name))
			return
		}
		if colors[name] == 2 {
			return
		}
		colors[name] = 1
		for _, dep := range g.Nodes[name].Dependencies {
			visit(dep)
		}
		colors[name] = 2
	}
	for _, name := range sortedKeys(g.Nodes) {
		visit(name)
	}
}

func (c *compiler) with(file, where string, bindings map[string]Binding, inputs map[string]Port, s portScope, deps map[string]bool) {
	for _, key := range sortedKeys(bindings) {
		p, ok := inputs[key]
		if !ok {
			c.semantic(file, where, fmt.Errorf("unknown body input %q", key))
			continue
		}
		b := bindings[key]
		c.binding(file, where+"/with/"+key, &b, &p, s, deps)
	}
	for key, p := range inputs {
		if _, ok := bindings[key]; !ok && p.IsRequired() && len(p.Default) == 0 {
			c.semantic(file, where, fmt.Errorf("required body input %q has no binding/default", key))
		}
	}
}

func (c *compiler) text(file, where string, s *TextSource) {
	if s.File == "" {
		return
	}
	data, ok := c.files[s.File]
	if !ok || !c.admitted[s.File] {
		c.add("TEXT_FILE", "package", file, where, fmt.Errorf("text file %q not admitted", s.File))
		return
	}
	if len(data) == 0 || len(data) > 1<<20 || !utf8.Valid(data) {
		c.add("TEXT_FILE", "package", file, where, fmt.Errorf("text source must be nonempty UTF-8, at most 1 MiB"))
		return
	}
	s.Text = string(data)
	s.File = ""
}
func (c *compiler) checkDuration(file, where, value string) {
	if value != "" {
		_, err := Duration(value)
		c.semantic(file, where, err)
	}
}
func (c *compiler) execution(file, where, kind string, defaults, value Execution) Execution {
	if value.Timeout == "" {
		value.Timeout = defaults.Timeout
	}
	if value.Timeout == "" {
		switch kind {
		case "human":
			value.Timeout = "24h"
		case "switch":
			value.Timeout = "1m"
		case "llm", "agent", "code", "tool":
			value.Timeout = "30m"
		}
	}
	c.checkDuration(file, where+"/execution/timeout", value.Timeout)
	retry := Retry{MaxAttempts: 1, Backoff: "1s"}
	if defaults.Retry != nil {
		retry = *defaults.Retry
		if retry.Backoff == "" {
			retry.Backoff = "1s"
		}
	}
	if value.Retry != nil {
		retry.MaxAttempts = value.Retry.MaxAttempts
		if value.Retry.Backoff != "" {
			retry.Backoff = value.Retry.Backoff
		}
	}
	value.Retry = &retry
	c.checkDuration(file, where+"/execution/retry/backoff", retry.Backoff)
	if contains([]string{"switch", "human", "foreach", "loop", "pipeline"}, kind) && retry.MaxAttempts != 1 {
		c.semantic(file, where, fmt.Errorf("%s does not allow whole-node retries", kind))
	}
	if value.OnUnknownOutcome == "" {
		value.OnUnknownOutcome = defaults.OnUnknownOutcome
	}
	if value.OnUnknownOutcome == "" {
		value.OnUnknownOutcome = "pause"
	}
	return value
}

func effectiveTools(defaults ToolGrants, local *ToolGrants) *ToolGrants {
	inherit := false
	out := &ToolGrants{MCP: map[string][]string{}, Inherit: &inherit}
	if local == nil || local.Inherit == nil || *local.Inherit {
		for k, v := range defaults.MCP {
			out.MCP[k] = append([]string{}, v...)
		}
		out.Sandbox = append([]string{}, defaults.Sandbox...)
	}
	if local != nil {
		for k, v := range local.MCP {
			out.MCP[k] = union(out.MCP[k], v)
		}
		out.Sandbox = union(out.Sandbox, local.Sandbox)
	}
	return out
}
func union(a, b []string) []string {
	m := map[string]bool{}
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		m[s] = true
	}
	return sortedKeys(m)
}

func rewriteRefs(v any, prefix string) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			if k == "$ref" {
				if s, ok := v.(string); ok && strings.HasPrefix(s, "#") {
					out[k] = "#" + prefix + strings.TrimPrefix(s, "#")
					continue
				}
			}
			switch k {
			case "$defs", "properties", "patternProperties", "dependentSchemas":
				if entries, ok := v.(map[string]any); ok {
					replaced := map[string]any{}
					for name, schema := range entries {
						replaced[name] = rewriteRefs(schema, prefix)
					}
					out[k] = replaced
				} else {
					out[k] = v
				}
			case "additionalProperties", "propertyNames", "items", "contains", "not", "if", "then", "else", "prefixItems", "allOf", "anyOf", "oneOf":
				out[k] = rewriteRefs(v, prefix)
			default:
				out[k] = v
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = rewriteRefs(v, prefix)
		}
		return out
	default:
		return v
	}
}
func wrapSchema(raw json.RawMessage, key string, outer map[string]any) json.RawMessage {
	s, _ := DecodeJSON(raw)
	outer[key] = rewriteRefs(s, "/"+key)
	out, _ := json.Marshal(outer)
	return out
}
