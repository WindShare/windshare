package main

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

type terminalBehavior uint8

const (
	terminalNormalReturn terminalBehavior = 1 << iota
	terminalPanicUnwind
	terminalGoexitUnwind
	terminalImmediateExit
	terminalNontermination
	terminalUnknown
)

const terminalSuppressesOlderDefers = terminalImmediateExit | terminalNontermination | terminalUnknown

type terminationSummary struct {
	possible terminalBehavior
}

func (summary terminationSummary) may(behavior terminalBehavior) bool {
	return summary.possible&behavior != 0
}

func (summary terminationSummary) isOnly(behavior terminalBehavior) bool {
	return summary.possible == behavior
}

// outcomeTransformer describes a direct deferred invocation, not just an
// ordinary call. That distinction is what makes recover compositional: only the
// deferred target receives the caller's active unwind as its incoming outcome;
// calls made by that target are ordinary calls and receive normal input.
type outcomeTransformer struct {
	fromNormal terminationSummary
	fromPanic  terminationSummary
	fromGoexit terminationSummary
}

func identityOutcomeTransformer() outcomeTransformer {
	return outcomeTransformer{
		fromNormal: terminationSummary{possible: terminalNormalReturn},
		fromPanic:  terminationSummary{possible: terminalPanicUnwind},
		fromGoexit: terminationSummary{possible: terminalGoexitUnwind},
	}
}

func fixedOutcomeTransformer(behavior terminalBehavior) outcomeTransformer {
	summary := terminationSummary{possible: behavior}
	return outcomeTransformer{
		fromNormal: summary,
		fromPanic:  summary,
		fromGoexit: summary,
	}
}

func unknownOutcomeTransformer() outcomeTransformer {
	return fixedOutcomeTransformer(terminalUnknown)
}

func (transformer outcomeTransformer) after(incoming terminalBehavior) terminationSummary {
	possible := incoming & terminalSuppressesOlderDefers
	if incoming&terminalNormalReturn != 0 {
		possible |= transformer.fromNormal.possible
	}
	if incoming&terminalPanicUnwind != 0 {
		possible |= transformer.fromPanic.possible
	}
	if incoming&terminalGoexitUnwind != 0 {
		possible |= transformer.fromGoexit.possible
	}
	return terminationSummary{possible: possible}
}

func (transformer outcomeTransformer) ordinaryCall() terminationSummary {
	return transformer.after(terminalNormalReturn)
}

func (transformer outcomeTransformer) normalized() outcomeTransformer {
	if transformer.fromNormal.possible == 0 {
		transformer.fromNormal.possible = terminalUnknown
	}
	if transformer.fromPanic.possible == 0 {
		transformer.fromPanic.possible = terminalUnknown
	}
	if transformer.fromGoexit.possible == 0 {
		transformer.fromGoexit.possible = terminalUnknown
	}
	return transformer
}

func (transformer *outcomeTransformer) include(other outcomeTransformer) {
	transformer.fromNormal.possible |= other.fromNormal.possible
	transformer.fromPanic.possible |= other.fromPanic.possible
	transformer.fromGoexit.possible |= other.fromGoexit.possible
}

// prepend composes a newly registered defer in front of the older stack. A
// panic started while Goexit is running is recoverable, but recovering it must
// resume Goexit. Keeping that rule in composition prevents a transient panic
// from accidentally turning an unreturning Goexit path into normal return.
func (older outcomeTransformer) prepend(newer outcomeTransformer) outcomeTransformer {
	return outcomeTransformer{
		fromNormal: older.afterDeferredTarget(newer.fromNormal, terminalNormalReturn),
		fromPanic:  older.afterDeferredTarget(newer.fromPanic, terminalPanicUnwind),
		fromGoexit: older.afterDeferredTarget(newer.fromGoexit, terminalGoexitUnwind),
	}
}

func (older outcomeTransformer) afterDeferredTarget(
	target terminationSummary,
	incoming terminalBehavior,
) terminationSummary {
	possible := target.possible & terminalSuppressesOlderDefers
	for _, outcome := range []terminalBehavior{
		terminalNormalReturn,
		terminalPanicUnwind,
		terminalGoexitUnwind,
	} {
		if !target.may(outcome) {
			continue
		}
		afterOlder := older.after(outcome)
		if incoming == terminalGoexitUnwind && outcome == terminalPanicUnwind &&
			afterOlder.may(terminalNormalReturn) {
			// Goexit remains suspended below a panic raised by a defer. Recovering
			// that panic resumes Goexit; recover never consumes Goexit itself.
			afterOlder.possible &^= terminalNormalReturn
			afterOlder.possible |= terminalGoexitUnwind
		}
		possible |= afterOlder.possible
	}
	return terminationSummary{possible: possible}
}

func (transformer outcomeTransformer) preservesOlderDefers() bool {
	for _, incoming := range []terminalBehavior{
		terminalNormalReturn,
		terminalPanicUnwind,
		terminalGoexitUnwind,
	} {
		outcomes := transformer.after(incoming).possible
		if outcomes == 0 || outcomes&terminalSuppressesOlderDefers != 0 {
			return false
		}
	}
	return true
}

type terminationPoint struct {
	block   *ssa.BasicBlock
	index   int
	defers  outcomeTransformer
	pending terminalBehavior
}

type instructionLocation struct {
	block *ssa.BasicBlock
	index int
}

type functionResolution uint8

const (
	functionResolving functionResolution = iota + 1
	functionResolved
)

type terminationResolver struct {
	resolved     map[ssa.CallInstruction][]*ssa.Function
	states       map[*ssa.Function]functionResolution
	transformers map[*ssa.Function]outcomeTransformer
}

// resolveOutcomeTransformers keeps normal calls, panic unwind, and Goexit
// unwind distinct at every interprocedural edge. Unknown dispatch and recursive
// paths remain explicit outcomes so neither caller continuation nor older defer
// execution can be inferred from an incomplete graph.
func resolveOutcomeTransformers(
	ordered []*ssa.Function,
	resolved map[ssa.CallInstruction][]*ssa.Function,
) map[*ssa.Function]outcomeTransformer {
	resolver := terminationResolver{
		resolved:     resolved,
		states:       make(map[*ssa.Function]functionResolution),
		transformers: make(map[*ssa.Function]outcomeTransformer),
	}
	for _, function := range ordered {
		resolver.functionTransformer(function)
	}
	return resolver.transformers
}

func (resolver *terminationResolver) functionTransformer(function *ssa.Function) outcomeTransformer {
	if function == nil {
		return unknownOutcomeTransformer()
	}
	if intrinsic := intrinsicTermination(functionObject(function)); intrinsic != 0 {
		transformer := fixedOutcomeTransformer(intrinsic)
		resolver.transformers[function] = transformer
		resolver.states[function] = functionResolved
		return transformer
	}
	switch resolver.states[function] {
	case functionResolving:
		// A compiler-visible call cycle can execute forever. Branches that leave
		// the SCC are accumulated independently by the enclosing CFG walk.
		return fixedOutcomeTransformer(terminalNontermination)
	case functionResolved:
		return resolver.transformers[function]
	}
	if len(function.Blocks) == 0 {
		transformer := unknownOutcomeTransformer()
		resolver.transformers[function] = transformer
		resolver.states[function] = functionResolved
		return transformer
	}

	resolver.states[function] = functionResolving
	if delegate, ok := transparentMethodDelegate(function); ok {
		// x/tools represents a language-level method value/expression with an
		// analysis-only tail-call frame. Go's recover rule applies to the method
		// body that was directly deferred, so carrying the incoming unwind through
		// this verified frame is required; a source helper remains an ordinary call.
		transformer := resolver.callTransformer(delegate).normalized()
		resolver.transformers[function] = transformer
		resolver.states[function] = functionResolved
		return transformer
	}
	transformer := outcomeTransformer{
		fromNormal: resolver.walkFunction(function, terminalNormalReturn),
		fromPanic:  resolver.walkFunction(function, terminalPanicUnwind),
		fromGoexit: resolver.walkFunction(function, terminalGoexitUnwind),
	}.normalized()
	resolver.transformers[function] = transformer
	resolver.states[function] = functionResolved
	return transformer
}

func (resolver *terminationResolver) walkFunction(
	function *ssa.Function,
	incoming terminalBehavior,
) terminationSummary {
	visits := make(map[terminationPoint]terminationSummary)
	visited := make(map[terminationPoint]bool)
	active := make(map[instructionLocation]bool)
	var walk func(terminationPoint) terminationSummary
	walk = func(point terminationPoint) terminationSummary {
		location := instructionLocation{block: point.block, index: point.index}
		if active[location] {
			// SSA cannot prove that runtime values force an exit from a reachable
			// back edge, so the cycle contributes a nontermination outcome.
			return terminationSummary{possible: terminalNontermination}
		}
		if visited[point] {
			return visits[point]
		}
		active[location] = true
		defer delete(active, location)

		var result terminationSummary
		block := point.block
		if point.index < len(block.Instrs) {
			instruction := block.Instrs[point.index]
			next := point
			next.index++
			switch instruction := instruction.(type) {
			case *ssa.Defer:
				next.defers = point.defers.prepend(resolver.callTransformer(instruction))
				result = walk(next)
			case *ssa.Return:
				result = resumePending(point.defers.after(terminalNormalReturn), point.pending)
			case *ssa.Panic:
				result = resumePending(point.defers.after(terminalPanicUnwind), point.pending)
			case *ssa.RunDefers:
				deferred := point.defers.after(terminalNormalReturn)
				result.possible = deferred.possible &^ terminalNormalReturn
				if deferred.may(terminalNormalReturn) {
					next.defers = identityOutcomeTransformer()
					result.possible |= walk(next).possible
				}
			case *ssa.Call:
				if isDirectRecoverCall(instruction) {
					if point.pending == terminalPanicUnwind {
						next.pending = terminalNormalReturn
					}
					result = walk(next)
					break
				}
				called := resolver.callTransformer(instruction).ordinaryCall()
				result.possible = called.possible & terminalSuppressesOlderDefers
				if called.may(terminalNormalReturn) {
					result.possible |= walk(next).possible
				}
				for _, unwind := range []terminalBehavior{
					terminalPanicUnwind,
					terminalGoexitUnwind,
				} {
					if called.may(unwind) {
						unwound := point.defers.after(unwind)
						result.possible |= resumePending(unwound, point.pending).possible
					}
				}
			default:
				if implicit, ok := implicitInstructionOutcome(instruction); ok {
					if implicit.may(terminalNormalReturn) {
						result.possible |= walk(next).possible
					}
					if implicit.may(terminalPanicUnwind) {
						unwound := point.defers.after(terminalPanicUnwind)
						result.possible |= resumePending(unwound, point.pending).possible
					}
				} else {
					result = walk(next)
				}
				if instructionMayBlock(instruction) {
					result.possible |= terminalNontermination
				}
			}
		} else if len(block.Succs) == 0 {
			result.possible = terminalNontermination
		} else {
			for _, successor := range block.Succs {
				next := point
				next.block = successor
				next.index = 0
				result.possible |= walk(next).possible
			}
		}
		visited[point] = true
		visits[point] = result
		return result
	}
	return walk(terminationPoint{
		block:   function.Blocks[0],
		defers:  identityOutcomeTransformer(),
		pending: incoming,
	})
}

func resumePending(completed terminationSummary, pending terminalBehavior) terminationSummary {
	possible := completed.possible &^ terminalNormalReturn
	if completed.may(terminalNormalReturn) {
		possible |= pending
	}
	return terminationSummary{possible: possible}
}

func isDirectRecoverCall(call *ssa.Call) bool {
	if call == nil || call.Common() == nil {
		return false
	}
	builtin, ok := call.Common().Value.(*ssa.Builtin)
	return ok && builtin.Name() == "recover"
}

func (resolver *terminationResolver) callTransformer(call ssa.CallInstruction) outcomeTransformer {
	common := call.Common()
	if common == nil {
		return unknownOutcomeTransformer()
	}
	if transformer, ok := builtinOutcomeTransformer(common); ok {
		return transformer
	}
	targets := callTargets(call, resolver.resolved)
	if len(targets) == 0 {
		return unknownOutcomeTransformer()
	}
	var transformer outcomeTransformer
	for _, target := range targets {
		transformer.include(resolver.functionTransformer(target))
	}
	return transformer.normalized()
}

func instructionMayBlock(instruction ssa.Instruction) bool {
	switch instruction := instruction.(type) {
	case *ssa.Select:
		return instruction.Blocking
	case *ssa.Send:
		return true
	case *ssa.UnOp:
		return instruction.Op == token.ARROW
	default:
		return false
	}
}

func callOutcomeTransformer(
	call ssa.CallInstruction,
	resolved map[ssa.CallInstruction][]*ssa.Function,
	transformers map[*ssa.Function]outcomeTransformer,
) outcomeTransformer {
	common := call.Common()
	if common == nil {
		return unknownOutcomeTransformer()
	}
	if transformer, ok := builtinOutcomeTransformer(common); ok {
		return transformer
	}
	targets := callTargets(call, resolved)
	if len(targets) == 0 {
		return unknownOutcomeTransformer()
	}
	var result outcomeTransformer
	for _, target := range targets {
		transformer := transformers[target]
		if transformer == (outcomeTransformer{}) {
			if intrinsic := intrinsicTermination(functionObject(target)); intrinsic != 0 {
				transformer = fixedOutcomeTransformer(intrinsic)
			} else {
				transformer = unknownOutcomeTransformer()
			}
		}
		result.include(transformer)
	}
	return result.normalized()
}

func intrinsicTermination(function *types.Func) terminalBehavior {
	if function == nil || function.Pkg() == nil || receiverTypeName(function) != "" {
		return 0
	}
	switch function.Pkg().Path() {
	case "os":
		if function.Name() == "Exit" {
			return terminalImmediateExit
		}
	case "runtime":
		switch function.Name() {
		case "Goexit":
			return terminalGoexitUnwind
		case "exit", "fatalthrow", "fatalpanic":
			return terminalImmediateExit
		}
	case "syscall":
		switch function.Name() {
		case "Exit", "ExitProcess", "ExitThread":
			return terminalImmediateExit
		}
	}
	return 0
}
