package main

import "golang.org/x/tools/go/ssa"

type instructionPoint struct {
	block          *ssa.BasicBlock
	index          int
	deferredEffect bool
	defers         outcomeTransformer
}

type pathVisit uint8

const (
	pathVisiting pathVisit = iota + 1
	pathProven
	pathRejected
)

func allReachablePathsFollowEffect(
	function *ssa.Function,
	effects map[ssa.Instruction]bool,
	resolved map[ssa.CallInstruction][]*ssa.Function,
	terminations map[*ssa.Function]outcomeTransformer,
) bool {
	return proveAllPaths(function, effects, resolved, terminations)
}

// proveAllPaths consumes the same outcome transformers as the interprocedural
// terminal resolver. A recovered helper can therefore prove its continuation,
// while an ambiguous recovery, unresolved call, or suppressing defer still
// rejects the universal resource-effect claim.
func proveAllPaths(
	function *ssa.Function,
	effects map[ssa.Instruction]bool,
	resolved map[ssa.CallInstruction][]*ssa.Function,
	terminations map[*ssa.Function]outcomeTransformer,
) bool {
	if function == nil || len(function.Blocks) == 0 {
		return false
	}
	visits := make(map[instructionPoint]pathVisit)
	var prove func(instructionPoint) bool
	prove = func(point instructionPoint) bool {
		switch visits[point] {
		case pathVisiting:
			// A reachable cycle can remain in the pre-effect graph forever. SSA
			// cannot prove that runtime values force a later exit from it.
			return false
		case pathProven:
			return true
		case pathRejected:
			return false
		}
		visits[point] = pathVisiting
		proven := false
		block := point.block
		if point.index < len(block.Instrs) {
			instruction := block.Instrs[point.index]
			next := point
			next.index++
			if deferred, ok := instruction.(*ssa.Defer); ok {
				target := callOutcomeTransformer(deferred, resolved, terminations)
				next.defers = point.defers.prepend(target)
				if effects[instruction] {
					// Scheduling is not the effect: process exit and an exit-less
					// path never invoke the defer. The pending marker is retained only
					// while every newer target must continue through older defers.
					next.deferredEffect = true
				} else if point.deferredEffect && !target.preservesOlderDefers() {
					next.deferredEffect = false
				}
				proven = prove(next)
			} else if effects[instruction] {
				proven = true
			} else if instructionMayBlock(instruction) {
				// A channel operation can remain blocked forever. Reaching it is
				// therefore not proof that a later effect or defer unwind executes.
				proven = false
			} else {
				switch instruction := instruction.(type) {
				case *ssa.Return, *ssa.Panic:
					proven = point.deferredEffect
				case *ssa.RunDefers:
					if point.deferredEffect {
						proven = true
					} else if point.defers.after(terminalNormalReturn).isOnly(terminalNormalReturn) {
						next.defers = identityOutcomeTransformer()
						proven = prove(next)
					}
				case *ssa.Call:
					called := callOutcomeTransformer(instruction, resolved, terminations).ordinaryCall()
					if called.isOnly(terminalNormalReturn) {
						proven = prove(next)
					} else if point.deferredEffect && called.possible != 0 &&
						called.possible&terminalSuppressesOlderDefers == 0 {
						// Panic and Goexit enter this function's defer stack. A normal
						// branch must still establish the effect through its continuation.
						proven = !called.may(terminalNormalReturn) || prove(next)
					}
				default:
					if implicit, ok := implicitInstructionOutcome(instruction); ok {
						if implicit.isOnly(terminalNormalReturn) {
							proven = prove(next)
						} else if point.deferredEffect {
							// An implicit panic still unwinds an already registered
							// effect; its successful branch must prove the continuation.
							proven = !implicit.may(terminalNormalReturn) || prove(next)
						}
					} else {
						proven = prove(next)
					}
				}
			}
		} else if len(block.Succs) != 0 {
			proven = true
			for _, successor := range block.Succs {
				next := point
				next.block = successor
				next.index = 0
				if !prove(next) {
					proven = false
					break
				}
			}
		}
		if proven {
			visits[point] = pathProven
		} else {
			visits[point] = pathRejected
		}
		return proven
	}
	return prove(instructionPoint{
		block:  function.Blocks[0],
		defers: identityOutcomeTransformer(),
	})
}
