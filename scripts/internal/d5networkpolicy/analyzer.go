package main

import (
	"fmt"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

const runtimeGatePackage = "github.com/windshare/windshare/internal/testnetwork"

func analyzeRoot(root string) (analysisResult, error) {
	fset := token.NewFileSet()
	configuration := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps,
		Dir:   root,
		Fset:  fset,
		Tests: true,
	}
	loaded, err := packages.Load(configuration, "./...")
	if err != nil {
		return analysisResult{}, err
	}
	if count := packages.PrintErrors(loaded); count != 0 {
		return analysisResult{}, fmt.Errorf("package loading reported %d errors", count)
	}
	program, _ := ssautil.AllPackages(loaded, ssa.InstantiateGenerics)
	program.Build()
	functions := ssautil.AllFunctions(program)
	resolvedTargets := callGraphTargets(vta.CallGraph(functions, nil))
	orderedFunctions := sortedFunctions(functions)
	terminations := resolveOutcomeTransformers(orderedFunctions, resolvedTargets)
	policies := make(map[*ssa.Function]*functionPolicy)
	for _, function := range orderedFunctions {
		if function == nil || len(function.Blocks) == 0 {
			continue
		}
		position := fset.Position(function.Pos())
		if position.Filename == "" || !pathUnderRoot(position.Filename, root) {
			continue
		}
		policy := &functionPolicy{
			function:  function,
			file:      position.Filename,
			testOwned: strings.HasSuffix(strings.ToLower(position.Filename), "_test.go"),
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(ssa.CallInstruction)
				if ok {
					policy.calls = append(policy.calls, call)
				}
			}
		}
		policies[function] = policy
	}

	gateFunctions := resolveGateFunctions(orderedFunctions, policies, resolvedTargets)
	authorization := make(map[*ssa.Function]map[ssa.Instruction]bool, len(policies))
	for function, policy := range policies {
		authorization[function], _ = authorizationState(policy, resolvedTargets, gateFunctions)
	}

	// Value-flow summaries cross transparent wrappers and recursive SCCs. Static
	// business calls are not promoted merely because some data-dependent branch
	// can eventually use the network; doing so would erase the distinction between
	// validation/fake paths and the manifest-controlled real-network suite.
	summaries := make(map[*ssa.Function]resourceSummary, len(policies))
	for {
		changed := false
		for _, function := range orderedFunctions {
			policy := policies[function]
			if policy == nil {
				continue
			}
			next := summarizeFunction(
				policy,
				resolvedTargets,
				summaries,
				authorization[function],
				terminations,
			)
			current := summaries[function]
			if next.reachesResource && !current.reachesResource {
				current.reachesResource = true
				changed = true
			}
			if next.requiresGate && !current.requiresGate {
				current.requiresGate = true
				changed = true
			}
			if next.valueFlow && !current.valueFlow {
				current.valueFlow = true
				changed = true
			}
			if next.transparentValueFlow && !current.transparentValueFlow {
				current.transparentValueFlow = true
				changed = true
			}
			summaries[function] = current
		}
		if !changed {
			break
		}
	}

	catalog, err := buildSemanticCatalog(root, policies, resolvedTargets)
	if err != nil {
		return analysisResult{}, fmt.Errorf("build semantic execution catalog: %w", err)
	}
	result := analysisResult{
		classified: make(map[string]bool),
		catalog:    catalog,
	}
	violationSet := make(map[string]bool)
	for _, function := range orderedFunctions {
		policy := policies[function]
		if policy == nil || !policy.testOwned || !summaries[function].reachesResource {
			continue
		}
		relativeDirectory, err := filepath.Rel(root, filepath.Dir(policy.file))
		if err != nil {
			return analysisResult{}, err
		}
		result.classified[filepath.ToSlash(relativeDirectory)] = true
		for _, call := range policy.calls {
			effect := callResourceEffect(policy, call, resolvedTargets, summaries)
			if !effect.hardBoundary || !effect.requiresGate || authorization[function][call] {
				continue
			}
			position := fset.Position(call.Pos())
			violation := fmt.Sprintf(
				"%s:%d: %s reaches semantic resource %s without a dominating runtime gate in the same owner",
				filepath.ToSlash(policy.file), position.Line, policy.function.String(), effect.primitive,
			)
			appendViolation(&result, violationSet, violation)
		}
	}
	for function := range testEntryFunctions(policies, resolvedTargets, summaries) {
		policy := policies[function]
		for _, call := range policy.calls {
			effect := callResourceEffect(policy, call, resolvedTargets, summaries)
			if effect.hardBoundary || !effect.requiresGate || authorization[function][call] {
				continue
			}
			position := fset.Position(call.Pos())
			violation := fmt.Sprintf(
				"%s:%d: test entry %s can reach production semantic resource %s before any authenticated runtime gate",
				filepath.ToSlash(policy.file), position.Line, policy.function.String(), effect.primitive,
			)
			appendViolation(&result, violationSet, violation)
		}
	}
	sort.Strings(result.violations)
	return result, nil
}

func appendViolation(result *analysisResult, seen map[string]bool, violation string) {
	if !seen[violation] {
		seen[violation] = true
		result.violations = append(result.violations, violation)
	}
}

func isRuntimeGate(function *types.Func) bool {
	if function == nil || function.Pkg() == nil || function.Pkg().Path() != runtimeGatePackage {
		return false
	}
	return function.Name() == "RequireOSNetwork" || function.Name() == "AssertOSNetwork"
}

// resourcePrimitive applies the audited effect catalog only after the compiler
// has resolved a call target to a types.Func. Source aliases, imports, caller
// names, and textual call spelling therefore cannot influence classification.
func resourcePrimitive(function *types.Func) string {
	if function == nil || function.Pkg() == nil {
		return ""
	}
	path := function.Pkg().Path()
	name := function.Name()
	receiver := receiverTypeName(function)
	resource := false
	switch path {
	case "net":
		if receiver == "" {
			resource = hasAnyPrefix(name, "Dial", "Listen", "Lookup") ||
				name == "FileConn" || name == "FileListener" || name == "FilePacketConn"
		} else {
			resource = (receiver == "Dialer" && hasAnyPrefix(name, "Dial")) ||
				(receiver == "ListenConfig" && hasAnyPrefix(name, "Listen")) ||
				(receiver == "Resolver" && hasAnyPrefix(name, "Lookup"))
		}
	case "net/http":
		if receiver == "" {
			resource = name == "Get" || name == "Head" || name == "Post" ||
				name == "PostForm" || name == "Serve" || hasAnyPrefix(name, "ListenAndServe")
		} else {
			resource = (receiver == "Client" && name == "Do") ||
				(receiver == "Transport" && name == "RoundTrip") ||
				(receiver == "Server" && (name == "Serve" || name == "ServeTLS"))
		}
	case "net/http/httptest":
		resource = name == "NewServer" || name == "NewTLSServer" || name == "NewUnstartedServer"
	case "os/exec":
		resource = (receiver == "" && hasAnyPrefix(name, "Command")) ||
			(receiver == "Cmd" && (name == "Start" || name == "Run" ||
				name == "Output" || name == "CombinedOutput"))
	case "github.com/coder/websocket":
		resource = name == "Dial"
	case "github.com/pion/webrtc/v4":
		resource = name == "NewPeerConnection"
	}
	if !resource {
		return ""
	}
	return path + "." + name
}

func receiverTypeName(function *types.Func) string {
	signature, _ := function.Type().(*types.Signature)
	if signature == nil || signature.Recv() == nil {
		return ""
	}
	typeValue := signature.Recv().Type()
	if pointer, ok := typeValue.(*types.Pointer); ok {
		typeValue = pointer.Elem()
	}
	named, _ := typeValue.(*types.Named)
	if named == nil || named.Obj() == nil {
		return ""
	}
	return named.Obj().Name()
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func pathUnderRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
