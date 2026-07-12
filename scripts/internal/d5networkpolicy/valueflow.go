package main

import (
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
)

type resourceEffect struct {
	primitive       string
	reachesResource bool
	requiresGate    bool
	hardBoundary    bool
	valueFlow       bool
}

type resourceSummary struct {
	reachesResource      bool
	requiresGate         bool
	valueFlow            bool
	transparentValueFlow bool
}

type functionPolicy struct {
	function  *ssa.Function
	file      string
	testOwned bool
	calls     []ssa.CallInstruction
}

func callGraphTargets(graph *callgraph.Graph) map[ssa.CallInstruction][]*ssa.Function {
	sets := make(map[ssa.CallInstruction]map[*ssa.Function]bool)
	for _, node := range graph.Nodes {
		for _, edge := range node.Out {
			if edge.Site == nil || edge.Callee == nil || edge.Callee.Func == nil {
				continue
			}
			if sets[edge.Site] == nil {
				sets[edge.Site] = make(map[*ssa.Function]bool)
			}
			sets[edge.Site][edge.Callee.Func] = true
		}
	}
	result := make(map[ssa.CallInstruction][]*ssa.Function, len(sets))
	for call, targets := range sets {
		for target := range targets {
			result[call] = append(result[call], target)
		}
		sort.Slice(result[call], func(left, right int) bool {
			return result[call][left].String() < result[call][right].String()
		})
	}
	return result
}

func callTargets(
	call ssa.CallInstruction,
	resolved map[ssa.CallInstruction][]*ssa.Function,
) []*ssa.Function {
	if callee := call.Common().StaticCallee(); callee != nil {
		return []*ssa.Function{callee}
	}
	return resolved[call]
}

func resolveGateFunctions(
	ordered []*ssa.Function,
	policies map[*ssa.Function]*functionPolicy,
	resolved map[ssa.CallInstruction][]*ssa.Function,
) map[*ssa.Function]bool {
	result := make(map[*ssa.Function]bool)
	for {
		changed := false
		for _, function := range ordered {
			policy := policies[function]
			if policy == nil || result[function] {
				continue
			}
			_, establishes := authorizationState(policy, resolved, result)
			if establishes {
				result[function] = true
				changed = true
			}
		}
		if !changed {
			return result
		}
	}
}

func authorizationState(
	policy *functionPolicy,
	resolved map[ssa.CallInstruction][]*ssa.Function,
	gateFunctions map[*ssa.Function]bool,
) (map[ssa.Instruction]bool, bool) {
	blockIn := make(map[*ssa.BasicBlock]bool, len(policy.function.Blocks))
	blockOut := make(map[*ssa.BasicBlock]bool, len(policy.function.Blocks))
	for _, block := range policy.function.Blocks {
		blockIn[block] = true
		blockOut[block] = true
	}
	entry := policy.function.Blocks[0]
	blockIn[entry] = false
	for {
		changed := false
		for _, block := range policy.function.Blocks {
			incoming := false
			if block != entry && len(block.Preds) != 0 {
				incoming = true
				for _, predecessor := range block.Preds {
					incoming = incoming && blockOut[predecessor]
				}
			}
			outgoing := incoming
			for _, instruction := range block.Instrs {
				call, ok := instruction.(*ssa.Call)
				if ok && callEstablishesGate(call, resolved, gateFunctions) {
					outgoing = true
				}
			}
			if blockIn[block] != incoming || blockOut[block] != outgoing {
				blockIn[block] = incoming
				blockOut[block] = outgoing
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	before := make(map[ssa.Instruction]bool)
	hasReturn := false
	allReturnsAuthorized := true
	for _, block := range policy.function.Blocks {
		authorized := blockIn[block]
		for _, instruction := range block.Instrs {
			before[instruction] = authorized
			if _, ok := instruction.(*ssa.Return); ok {
				hasReturn = true
				allReturnsAuthorized = allReturnsAuthorized && authorized
			}
			call, ok := instruction.(*ssa.Call)
			if ok && callEstablishesGate(call, resolved, gateFunctions) {
				authorized = true
			}
		}
	}
	return before, hasReturn && allReturnsAuthorized
}

func callEstablishesGate(
	call *ssa.Call,
	resolved map[ssa.CallInstruction][]*ssa.Function,
	gateFunctions map[*ssa.Function]bool,
) bool {
	targets := callTargets(call, resolved)
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		if !isRuntimeGate(functionObject(target)) && !gateFunctions[target] {
			return false
		}
	}
	return true
}

func functionObject(function *ssa.Function) *types.Func {
	for function != nil {
		if object, ok := function.Object().(*types.Func); ok {
			return object
		}
		function = function.Origin()
	}
	return nil
}

func summarizeFunction(
	policy *functionPolicy,
	resolved map[ssa.CallInstruction][]*ssa.Function,
	summaries map[*ssa.Function]resourceSummary,
	authorized map[ssa.Instruction]bool,
	terminations map[*ssa.Function]outcomeTransformer,
) resourceSummary {
	var summary resourceSummary
	valueFlowCalls := make(map[ssa.Instruction]bool)
	for _, call := range policy.calls {
		effect := callResourceEffect(policy, call, resolved, summaries)
		summary.reachesResource = summary.reachesResource || effect.reachesResource
		summary.valueFlow = summary.valueFlow || effect.valueFlow
		valueFlowCalls[call] = effect.reachesResource && effect.valueFlow
		if effect.requiresGate && !authorized[call] {
			summary.requiresGate = true
		}
	}
	summary.transparentValueFlow = allReachablePathsFollowEffect(
		policy.function,
		valueFlowCalls,
		resolved,
		terminations,
	)
	return summary
}

func callResourceEffect(
	policy *functionPolicy,
	call ssa.CallInstruction,
	resolved map[ssa.CallInstruction][]*ssa.Function,
	summaries map[*ssa.Function]resourceSummary,
) resourceEffect {
	targets := callTargets(call, resolved)
	effect := resourceEffect{}
	unsafeTargets := make(map[string]bool)
	for _, target := range targets {
		if primitive := resourcePrimitive(functionObject(target)); primitive != "" {
			effect.reachesResource = true
			effect.requiresGate = true
			effect.hardBoundary = policy.testOwned
			effect.valueFlow = effect.valueFlow || isDynamicCall(call)
			unsafeTargets[primitive] = true
			continue
		}
		summary := summaries[target]
		// A dynamic call selects this exact VTA target, whereas a static call is
		// part of the bounded value-flow proof only when every reachable callee
		// behavior traverses its resource-bearing value.
		if !isDynamicCall(call) && !summary.transparentValueFlow {
			continue
		}
		if summary.reachesResource {
			effect.reachesResource = true
			effect.valueFlow = true
		}
		if summary.requiresGate {
			effect.requiresGate = true
			effect.hardBoundary = effect.hardBoundary || (policy.testOwned && isDynamicCall(call))
			unsafeTargets[target.String()] = true
		}
	}
	if isDynamicCall(call) && len(targets) == 0 {
		effect.reachesResource = true
		effect.requiresGate = true
		effect.hardBoundary = policy.testOwned
		effect.valueFlow = true
		unsafeTargets["unresolved dynamic call target"] = true
	}
	if len(unsafeTargets) != 0 {
		names := make([]string, 0, len(unsafeTargets))
		for name := range unsafeTargets {
			names = append(names, name)
		}
		sort.Strings(names)
		effect.primitive = strings.Join(names, " or ")
	}
	return effect
}

func testEntryFunctions(
	policies map[*ssa.Function]*functionPolicy,
	resolved map[ssa.CallInstruction][]*ssa.Function,
	summaries map[*ssa.Function]resourceSummary,
) map[*ssa.Function]bool {
	incoming := make(map[*ssa.Function]bool)
	externalIncoming := make(map[*ssa.Function]bool)
	// Compiler-resolved incoming edges from the generated test runner preserve
	// independent entry authority without inferring entry points from function names.
	for call, targets := range resolved {
		if policies[call.Parent()] != nil {
			continue
		}
		for _, target := range targets {
			if targetPolicy := policies[target]; targetPolicy != nil && targetPolicy.testOwned {
				externalIncoming[target] = true
			}
		}
	}
	for _, policy := range policies {
		if !policy.testOwned {
			continue
		}
		for _, call := range policy.calls {
			for _, target := range callTargets(call, resolved) {
				if targetPolicy := policies[target]; targetPolicy != nil && targetPolicy.testOwned {
					incoming[target] = true
				}
			}
		}
	}
	entries := make(map[*ssa.Function]bool)
	for function, policy := range policies {
		if !policy.testOwned || !summaries[function].reachesResource {
			continue
		}
		if externalIncoming[function] || !incoming[function] {
			entries[function] = true
		}
	}
	return entries
}

func isDynamicCall(call ssa.CallInstruction) bool {
	common := call.Common()
	if common == nil || common.StaticCallee() != nil {
		return false
	}
	_, builtin := common.Value.(*ssa.Builtin)
	return !builtin
}

func sortedFunctions(functions map[*ssa.Function]bool) []*ssa.Function {
	result := make([]*ssa.Function, 0, len(functions))
	for function, included := range functions {
		if included && function != nil {
			result = append(result, function)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		leftName, rightName := result[left].String(), result[right].String()
		if leftName != rightName {
			return leftName < rightName
		}
		return result[left].Pos() < result[right].Pos()
	})
	return result
}
