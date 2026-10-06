package contract

import (
	"cmp"
	"slices"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/interpreter"
)

// Go map iteration must never select a different macro result during Temporal
// replay. Map literals bypass type adaptation, so their constructors need
// wrappers. Attributes instead use the adapter below: wrapping a qualified attribute
// would hide cel-go's existing observation wrapper and count its lookup twice.
func orderedMapConstructors(step interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
	switch value := step.(type) {
	case interpreter.InterpretableConst:
		if value.Value().Type() == types.MapType {
			return orderedConstant{value}, nil
		}
	case interpreter.InterpretableConstructor:
		if value.Type() == types.MapType {
			return orderedConstructor{value}, nil
		}
	}

	return step, nil
}

type orderedConstant struct{ interpreter.InterpretableConst }

func (s orderedConstant) Exec(f *interpreter.ExecutionFrame) ref.Val {
	return orderedRange(s.InterpretableConst.Exec(f))
}

func (s orderedConstant) Eval(a interpreter.Activation) ref.Val {
	return orderedRange(s.InterpretableConst.Eval(a))
}

type orderedConstructor struct {
	interpreter.InterpretableConstructor
}

func (s orderedConstructor) Exec(f *interpreter.ExecutionFrame) ref.Val {
	return orderedRange(s.InterpretableConstructor.Exec(f))
}

func (s orderedConstructor) Eval(a interpreter.Activation) ref.Val {
	return orderedRange(s.InterpretableConstructor.Eval(a))
}

// Adapt attribute results, including native inputs, macro-local variables and
// maps obtained from conditionals or indexing. No cost instruction is replaced.
type orderedAdapter struct{}

func (orderedAdapter) NativeToValue(value any) ref.Val {
	return orderedRange(types.DefaultTypeAdapter.NativeToValue(value))
}

type orderedMap struct {
	traits.Mapper
}

func orderedRange(value ref.Val) ref.Val {
	if _, ok := value.(*orderedMap); ok {
		return value
	}
	mapping, ok := value.(traits.Mapper)
	if !ok {
		return value
	}
	// Native JSON objects have string keys by construction. CEL literals may
	// use dynamic keys, so validate every key, including a one-entry literal
	// that would never call the sort comparator. Literal sizes are AST-bounded.
	switch mapping.Value().(type) {
	case map[string]any, map[string]string:
	default:
		iterator := mapping.Iterator()
		for iterator.HasNext() == types.True {
			switch iterator.Next().(type) {
			case types.Bool, types.Int, types.Uint, types.String:
			default:
				return types.NewErr("CEL map keys must be bool, int, uint or string")
			}
		}
	}

	return &orderedMap{Mapper: mapping}
}

func compareCELKeys(a, b ref.Val) int {
	if order := cmp.Compare(a.Type().TypeName(), b.Type().TypeName()); order != 0 {
		return order
	}

	// Only CEL's scalar map keys have a deterministic total order.
	switch a.(type) {
	case types.Bool, types.Int, types.Uint, types.String:
	default:
		panic("unexpected CEL map key type")
	}
	comparer, ok := a.(traits.Comparer)
	if !ok {
		panic("CEL map key does not support comparison")
	}
	order, ok := comparer.Compare(b).(types.Int)
	if !ok {
		panic("incompatible CEL map keys")
	}
	return int(order)
}

func (m *orderedMap) Iterator() traits.Iterator {
	return types.NewRefValList(types.DefaultTypeAdapter, m.sortedKeys()).Iterator()
}

func (m *orderedMap) Fold(folder traits.Folder) {
	for _, key := range m.sortedKeys() {
		if !folder.FoldEntry(key, m.Get(key)) {
			return
		}
	}
}

// Copy keys only when a macro actually iterates. Qualifying a field does not
// sort its surrounding map, and concurrent evaluations share no mutable cache.
// orderedRange validates the key types before constructing this wrapper.
func (m *orderedMap) sortedKeys() []ref.Val {
	keys := []ref.Val{}
	iterator := m.Mapper.Iterator()

	for iterator.HasNext() == types.True {
		keys = append(keys, iterator.Next())
	}

	slices.SortFunc(keys, compareCELKeys)
	return keys
}
