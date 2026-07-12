package main

import (
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

const (
	ssaBoundMethodWrapperPrefix = "bound method wrapper for "
	ssaMethodThunkPrefix        = "thunk for "
	ssaWrapperNilCheckBuiltin   = "ssa:wrapnilchk"
)

type nilness uint8

const (
	nilnessUnknown nilness = iota
	nilnessNil
	nilnessNonNil
)

// transparentMethodDelegate recognizes only x/tools' structurally trivial
// method-value and method-expression frames. Synthetic provenance alone is not
// enough: requiring the receiver to flow directly into the sole tail call keeps
// receiver binding and interface dispatch intact, and makes future lowering
// changes fail closed instead of accidentally granting helper-like code recover
// authority.
func transparentMethodDelegate(function *ssa.Function) (*ssa.Call, bool) {
	if function == nil || function.Signature == nil || function.Signature.Recv() != nil ||
		len(function.Blocks) != 1 {
		return nil, false
	}
	object, ok := function.Object().(*types.Func)
	if !ok {
		return nil, false
	}
	signature, _ := object.Type().(*types.Signature)
	if signature == nil || signature.Recv() == nil {
		return nil, false
	}

	var receiver ssa.Value
	switch {
	case strings.HasPrefix(function.Synthetic, ssaBoundMethodWrapperPrefix) &&
		strings.HasSuffix(function.Name(), "$bound") && len(function.FreeVars) == 1:
		receiver = function.FreeVars[0]
	case strings.HasPrefix(function.Synthetic, ssaMethodThunkPrefix) &&
		strings.HasSuffix(function.Name(), "$thunk") && len(function.FreeVars) == 0 &&
		len(function.Params) != 0:
		receiver = function.Params[0]
	default:
		return nil, false
	}

	instructions := function.Blocks[0].Instrs
	if len(instructions) != 2 {
		return nil, false
	}
	delegate, ok := instructions[0].(*ssa.Call)
	if !ok {
		return nil, false
	}
	if _, ok := instructions[1].(*ssa.Return); !ok {
		return nil, false
	}
	common := delegate.Common()
	if common == nil {
		return nil, false
	}
	if common.IsInvoke() {
		return delegate, common.Value == receiver && common.Method == object
	}
	return delegate, len(common.Args) != 0 && common.Args[0] == receiver
}

func implicitInstructionOutcome(instruction ssa.Instruction) (terminationSummary, bool) {
	switch instruction := instruction.(type) {
	case *ssa.TypeAssert:
		return typeAssertOutcome(instruction), true
	case *ssa.UnOp:
		if instruction.Op == token.MUL {
			return pointerAccessOutcome(instruction.X), true
		}
	case *ssa.FieldAddr:
		return pointerAccessOutcome(instruction.X), true
	}
	return terminationSummary{}, false
}

func typeAssertOutcome(assertion *ssa.TypeAssert) terminationSummary {
	if assertion == nil || assertion.CommaOk {
		return terminationSummary{possible: terminalNormalReturn}
	}
	switch ssaValueNilness(assertion.X, make(map[ssa.Value]bool)) {
	case nilnessNil:
		return terminationSummary{possible: terminalPanicUnwind}
	case nilnessNonNil:
		if types.Identical(assertion.X.Type(), assertion.AssertedType) {
			return terminationSummary{possible: terminalNormalReturn}
		}
	}
	return terminationSummary{possible: terminalNormalReturn | terminalPanicUnwind}
}

func pointerAccessOutcome(pointer ssa.Value) terminationSummary {
	switch ssaValueNilness(pointer, make(map[ssa.Value]bool)) {
	case nilnessNil:
		return terminationSummary{possible: terminalPanicUnwind}
	case nilnessNonNil:
		return terminationSummary{possible: terminalNormalReturn}
	default:
		return terminationSummary{possible: terminalNormalReturn | terminalPanicUnwind}
	}
}

func ssaValueNilness(value ssa.Value, active map[ssa.Value]bool) nilness {
	if value == nil || active[value] {
		return nilnessUnknown
	}
	active[value] = true
	defer delete(active, value)
	switch value := value.(type) {
	case *ssa.Const:
		if value.IsNil() {
			return nilnessNil
		}
		return nilnessNonNil
	case *ssa.MakeInterface, *ssa.Alloc, *ssa.Global, *ssa.FieldAddr, *ssa.IndexAddr:
		return nilnessNonNil
	case *ssa.ChangeInterface:
		return ssaValueNilness(value.X, active)
	case *ssa.ChangeType:
		return ssaValueNilness(value.X, active)
	case *ssa.Convert:
		return ssaValueNilness(value.X, active)
	case *ssa.Phi:
		result := nilnessUnknown
		for _, edge := range value.Edges {
			edgeNilness := ssaValueNilness(edge, active)
			if edgeNilness == nilnessUnknown {
				return nilnessUnknown
			}
			if result != nilnessUnknown && result != edgeNilness {
				return nilnessUnknown
			}
			result = edgeNilness
		}
		return result
	default:
		return nilnessUnknown
	}
}

func builtinOutcomeTransformer(common *ssa.CallCommon) (outcomeTransformer, bool) {
	builtin, ok := common.Value.(*ssa.Builtin)
	if !ok {
		return outcomeTransformer{}, false
	}
	switch builtin.Name() {
	case "panic":
		return fixedOutcomeTransformer(terminalPanicUnwind), true
	case ssaWrapperNilCheckBuiltin:
		if len(common.Args) != 0 {
			switch ssaValueNilness(common.Args[0], make(map[ssa.Value]bool)) {
			case nilnessNil:
				return fixedOutcomeTransformer(terminalPanicUnwind), true
			case nilnessNonNil:
				return identityOutcomeTransformer(), true
			}
		}
		// The check is emitted while adapting a pointer to a value receiver.
		// Unless SSA proves nil, both the successful binding and the panic that
		// prevents defer registration must remain visible.
		return fixedOutcomeTransformer(terminalNormalReturn | terminalPanicUnwind), true
	default:
		// A builtin used as the defer target is not a function body directly
		// executing recover. All other normally returning builtins preserve input.
		return identityOutcomeTransformer(), true
	}
}
