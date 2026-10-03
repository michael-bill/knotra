package contract

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// Scope is one lexical evaluation frame. Nested graphs receive a new Scope;
// parent data enters exclusively through the declared `with` bindings.
type Scope struct {
	Inputs, Args, State, Body Values
	Nodes                     map[string]Values
	IterationItem             *Value
	IterationIndex            int
}

func pointer(v any, path string) (any, bool, error) {
	if path == "" {
		return v, true, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false, fmt.Errorf("invalid JSON Pointer")
	}
	segments := []string{}

	for _, part := range strings.Split(path[1:], "/") {
		var segment strings.Builder

		for i := 0; i < len(part); i++ {
			if part[i] != '~' {
				segment.WriteByte(part[i])
				continue
			}
			i++
			if i >= len(part) || (part[i] != '0' && part[i] != '1') {
				return nil, false, fmt.Errorf("invalid JSON Pointer escape")
			}
			if part[i] == '0' {
				segment.WriteByte('~')
			} else {
				segment.WriteByte('/')
			}
		}

		segments = append(segments, segment.String())
	}

	for _, key := range segments {
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[key]
			if !ok {
				return nil, false, nil
			}
		case []any:
			if key == "" || (len(key) > 1 && key[0] == '0') || strings.IndexFunc(key, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return nil, false, fmt.Errorf("invalid JSON Pointer array index %q", key)
			}
			i, e := strconv.ParseUint(key, 10, 64)
			if e != nil || i >= uint64(len(x)) {
				return nil, false, nil
			}
			v = x[i]
		default:
			return nil, false, fmt.Errorf("JSON Pointer traverses scalar or null")
		}
	}

	return v, true, nil
}

func lookup(source string, s Scope) (Value, bool, error) {
	p := strings.Split(source, ".")
	var values Values
	var key string
	if len(p) < 2 {
		return Value{}, false, fmt.Errorf("invalid reference %q", source)
	}

	switch p[0] {
	case "inputs":
		values = s.Inputs
		key = p[1]
	case "args":
		values = s.Args
		key = p[1]
	case "state":
		values = s.State
		key = p[1]
	case "nodes":
		if len(p) != 4 || p[2] != "outputs" {
			return Value{}, false, fmt.Errorf("invalid node reference")
		}
		values = s.Nodes[p[1]]
		key = p[3]
	case "body":
		if len(p) != 3 || p[1] != "outputs" {
			return Value{}, false, fmt.Errorf("invalid body reference")
		}
		values = s.Body
		key = p[2]
	case "iteration":
		if len(p) != 2 {
			return Value{}, false, fmt.Errorf("invalid iteration reference")
		}
		if p[1] == "index" {
			v, err := JSONValue(int64(s.IterationIndex))
			return v, true, err
		}
		if p[1] == "item" && s.IterationItem != nil {
			return *s.IterationItem, true, nil
		}
		return Value{}, false, nil
	default:
		return Value{}, false, fmt.Errorf("unknown scope %q", p[0])
	}

	v, ok := values[key]
	return v, ok, nil
}

// EvalBinding returns (value, present, error); JSON null remains present.
func EvalBinding(b Binding, s Scope) (Value, bool, error) {
	if len(b.Coalesce) > 0 {
		for _, candidate := range b.Coalesce {
			v, ok, err := EvalBinding(candidate, s)
			if err != nil {
				return Value{}, false, err
			}
			if ok {
				return v, true, nil
			}
		}

		return Value{}, false, nil
	}
	if b.From != "" {
		v, ok, err := lookup(b.From, s)
		if err != nil || !ok || b.Path == "" {
			return v, ok, err
		}
		if len(v.JSON) == 0 {
			return Value{}, false, fmt.Errorf("JSON Pointer on artifact")
		}
		decoded, err := DecodeJSON(v.JSON)
		if err != nil {
			return Value{}, false, err
		}
		item, ok, err := pointer(decoded, b.Path)
		if err != nil || !ok {
			return Value{}, ok, err
		}
		v, err = JSONValue(item)
		return v, true, err
	}
	if b.Expr != "" {
		return eval(b.Expr, s)
	}
	if len(b.Value) > 0 {
		v, err := DecodeJSON(b.Value)
		if err != nil {
			return Value{}, false, err
		}
		out, err := JSONValue(v)
		return out, true, err
	}
	return Value{}, false, fmt.Errorf("binding has no selector")
}

// EvalBool evaluates a checked boolean condition and preserves source absence.
func EvalBool(expression string, s Scope) (bool, bool, error) {
	if expression == "" {
		return true, true, nil
	}
	v, ok, err := eval(expression, s)
	if err != nil || !ok {
		return false, ok, err
	}
	decoded, err := DecodeJSON(v.JSON)
	if err != nil {
		return false, false, err
	}
	b, isBool := decoded.(bool)
	if !isBool {
		return false, true, fmt.Errorf("condition must produce bool")
	}
	return b, true, nil
}

type checkedExpression struct {
	ast     *cel.Ast
	program cel.Program
	refs    []string
}

var expressions sync.Map

func expression(source string) (*checkedExpression, error) {
	if cached, ok := expressions.Load(source); ok {
		return cached.(*checkedExpression), nil
	}
	if len(source) > 8192 {
		return nil, fmt.Errorf("CEL expression exceeds 8192 bytes")
	}
	opts := []cel.EnvOption{cel.CustomTypeAdapter(orderedAdapter{})}

	for _, name := range []string{"inputs", "args", "nodes", "state", "iteration", "body"} {
		opts = append(opts, cel.Variable(name, cel.DynType))
	}

	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, err
	}
	a, issues := env.Compile(source)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	proto, err := cel.AstToCheckedExpr(a)
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	count := 0
	if err := checkExpr(proto.Expr, refs, &count); err != nil {
		return nil, err
	}
	if count > 4096 {
		return nil, fmt.Errorf("CEL AST exceeds 4096 nodes")
	}
	program, err := env.Program(a, cel.CostLimit(100000), cel.CustomDecoratorV2(orderedMapConstructors))
	if err != nil {
		return nil, err
	}
	x := &checkedExpression{ast: a, program: program, refs: sortedKeys(refs)}
	expressions.Store(source, x)
	return x, nil
}

var namespaces = map[string]bool{
	"inputs":    true,
	"args":      true,
	"nodes":     true,
	"state":     true,
	"iteration": true,
	"body":      true,
}

var allowedFunctions = func() map[string]bool {
	m := map[string]bool{}

	for _, s := range strings.Fields("_+_ _-_ _*_ _/_ _%_ _==_ _!=_ _<_ _<=_ _>_ _>=_ _&&_ _||_ !_ -_ _?_:_ _[_] @in _in_ @not_strictly_false size int double string bool type contains startsWith endsWith matches") {
		m[s] = true
	}

	return m
}()

// accessChain recognizes field and literal-index access from its AST rather
// than scanning source text; strings and macro-local variables cannot spoof it.
func accessChain(e *exprpb.Expr) (string, []string, []*exprpb.Expr, bool) {
	if id := e.GetIdentExpr(); id != nil {
		return id.Name, nil, nil, true
	}
	if s := e.GetSelectExpr(); s != nil {
		root, parts, dyn, ok := accessChain(s.Operand)
		return root, append(parts, s.Field), dyn, ok
	}
	if c := e.GetCallExpr(); c != nil && c.Function == "_[_]" && len(c.Args) == 2 {
		root, parts, dyn, ok := accessChain(c.Args[0])
		key := ""
		if v := c.Args[1].GetConstExpr(); v != nil {
			if s, yes := v.ConstantKind.(*exprpb.Constant_StringValue); yes {
				key = s.StringValue
			} else {
				key = "\x00"
			}
		} else {
			key = "\x00"
			dyn = append(dyn, c.Args[1])
		}
		return root, append(parts, key), dyn, ok
	}
	return "", nil, nil, false
}

func checkExpr(e *exprpb.Expr, refs map[string]bool, count *int) error {
	if e == nil {
		return nil
	}
	*count++
	if root, parts, dynamic, ok := accessChain(e); ok && namespaces[root] {
		length := 1
		if root == "nodes" {
			length = 3
		}
		if root == "body" {
			length = 2
		}
		if len(parts) < length {
			return fmt.Errorf("whole namespace %s cannot be used as a value", root)
		}

		for _, p := range parts[:length] {
			if p == "\x00" {
				return fmt.Errorf("computed namespace/port name is forbidden")
			}
		}

		if root == "nodes" && parts[1] != "outputs" || root == "body" && parts[0] != "outputs" {
			return fmt.Errorf("expected outputs namespace")
		}
		if root == "iteration" && parts[0] != "item" && parts[0] != "index" {
			return fmt.Errorf("unknown iteration field")
		}
		refs[strings.Join(append([]string{root}, parts[:length]...), ".")] = true
		*count += len(parts)

		for _, d := range dynamic {
			if err := checkExpr(d, refs, count); err != nil {
				return err
			}
		}

		return nil
	}
	visit := func(child *exprpb.Expr) error { return checkExpr(child, refs, count) }

	switch x := e.ExprKind.(type) {
	case *exprpb.Expr_ConstExpr:
		switch x.ConstExpr.ConstantKind.(type) {
		case *exprpb.Constant_Uint64Value, *exprpb.Constant_BytesValue, *exprpb.Constant_DurationValue, *exprpb.Constant_TimestampValue:
			return fmt.Errorf("unsupported CEL literal type")
		}
	case *exprpb.Expr_IdentExpr:
		if contains([]string{"uint", "bytes", "timestamp", "duration"}, x.IdentExpr.Name) {
			return fmt.Errorf("unsupported CEL type")
		}
	case *exprpb.Expr_SelectExpr:
		return visit(x.SelectExpr.Operand)
	case *exprpb.Expr_CallExpr:
		if !allowedFunctions[x.CallExpr.Function] {
			return fmt.Errorf("unsupported CEL function %s", x.CallExpr.Function)
		}
		if err := visit(x.CallExpr.Target); err != nil {
			return err
		}
		for _, arg := range x.CallExpr.Args {
			if err := visit(arg); err != nil {
				return err
			}
		}
	case *exprpb.Expr_ListExpr:
		for _, el := range x.ListExpr.Elements {
			if err := visit(el); err != nil {
				return err
			}
		}
	case *exprpb.Expr_StructExpr:
		if x.StructExpr.MessageName != "" {
			return fmt.Errorf("CEL proto objects forbidden")
		}
		for _, entry := range x.StructExpr.Entries {
			if err := visit(entry.GetMapKey()); err != nil {
				return err
			}
			if err := visit(entry.Value); err != nil {
				return err
			}
		}
	case *exprpb.Expr_ComprehensionExpr:
		c := x.ComprehensionExpr
		if namespaces[c.IterVar] || namespaces[c.AccuVar] {
			return fmt.Errorf("macro cannot shadow a namespace")
		}
		for _, child := range []*exprpb.Expr{c.IterRange, c.AccuInit, c.LoopCondition, c.LoopStep, c.Result} {
			if err := visit(child); err != nil {
				return err
			}
		}
	}

	return nil
}

func eval(source string, s Scope) (Value, bool, error) {
	x, err := expression(source)
	if err != nil {
		return Value{}, false, err
	}
	// Missing graph sources dominate has/short-circuit expressions. Optional args
	// stay absent in activation, so has(args.x) remains usable as documented.
	for _, r := range x.refs {
		v, ok, err := lookup(r, s)
		if err != nil {
			return Value{}, false, err
		}
		if !ok && !strings.HasPrefix(r, "args.") {
			return Value{}, false, nil
		}
		if ok && len(v.JSON) == 0 {
			return Value{}, false, fmt.Errorf("artifacts cannot enter CEL")
		}
	}

	values := func(v Values) (map[string]any, error) {
		m := map[string]any{}

		for _, k := range sortedKeys(v) {
			if len(v[k].JSON) == 0 {
				continue
			}
			x, err := DecodeJSON(v[k].JSON)
			if err != nil {
				return nil, err
			}
			m[k] = x
		}

		return m, nil
	}
	activation := map[string]any{}

	for key, v := range map[string]Values{"inputs": s.Inputs, "args": s.Args, "state": s.State} {
		m, err := values(v)
		if err != nil {
			return Value{}, false, err
		}
		activation[key] = m
	}

	nodes := map[string]any{}

	for _, name := range sortedKeys(s.Nodes) {
		m, err := values(s.Nodes[name])
		if err != nil {
			return Value{}, false, err
		}
		nodes[name] = map[string]any{"outputs": m}
	}

	activation["nodes"] = nodes
	body, err := values(s.Body)
	if err != nil {
		return Value{}, false, err
	}
	activation["body"] = map[string]any{"outputs": body}
	iteration := map[string]any{"index": int64(s.IterationIndex)}
	if s.IterationItem != nil && len(s.IterationItem.JSON) > 0 {
		item, err := DecodeJSON(s.IterationItem.JSON)
		if err != nil {
			return Value{}, false, err
		}
		iteration["item"] = item
	}
	activation["iteration"] = iteration
	result, _, err := x.program.Eval(activation)
	if err != nil {
		return Value{}, false, err
	}
	native, err := celJSON(result)
	if err != nil {
		return Value{}, false, err
	}
	v, err := JSONValue(native)
	return v, true, err
}

func celJSON(v ref.Val) (any, error) {
	switch x := v.(type) {
	case types.Null:
		return nil, nil
	case types.Bool:
		return bool(x), nil
	case types.Int:
		return int64(x), nil
	case types.Double:
		return float64(x), nil
	case types.String:
		return string(x), nil
	case traits.Lister:
		out := []any{}
		for i := types.Int(0); i < x.Size().(types.Int); i++ {
			el, err := celJSON(x.Get(i))
			if err != nil {
				return nil, err
			}
			out = append(out, el)
		}
		return out, nil
	case traits.Mapper:
		out := map[string]any{}
		it := x.Iterator()
		for it.HasNext() == types.True {
			key := it.Next()
			s, ok := key.(types.String)
			if !ok {
				return nil, fmt.Errorf("CEL result map keys must be strings")
			}
			value, err := celJSON(x.Get(key))
			if err != nil {
				return nil, err
			}
			out[string(s)] = value
		}
		return out, nil
	}

	return nil, fmt.Errorf("CEL result %s is not JSON", v.Type())
}
