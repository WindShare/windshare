package main

import (
	"fmt"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/ssa"
)

const testingPackagePath = "testing"

type semanticEntry struct {
	PackagePath     string `json:"PackagePath"`
	Kind            string `json:"Kind"`
	Name            string `json:"Name"`
	RequiresNetwork bool   `json:"RequiresOSNetwork"`
}

type semanticCatalog struct {
	entries   []semanticEntry
	lifecycle map[string]bool
}

type reachabilityState struct {
	function *ssa.Function
	bindings map[*ssa.Parameter][]*ssa.Function
}

func buildSemanticCatalog(
	root string,
	policies map[*ssa.Function]*functionPolicy,
	resolved map[ssa.CallInstruction][]*ssa.Function,
) (semanticCatalog, error) {
	catalog := semanticCatalog{lifecycle: make(map[string]bool)}
	entryByKey := make(map[string]semanticEntry)
	for function, policy := range policies {
		if policy == nil || !policy.testOwned || function.Parent() != nil {
			continue
		}
		kind := testingEntryKind(function)
		if kind == "" && !isTestingLifecycle(function) {
			continue
		}
		packagePath, err := relativePackagePath(root, policy.file)
		if err != nil {
			return semanticCatalog{}, err
		}
		requiresNetwork := functionReachesRuntimeGate(function, policies, resolved)
		if kind == "" {
			catalog.lifecycle[packagePath] = catalog.lifecycle[packagePath] || requiresNetwork
			continue
		}
		name := function.Name()
		key := packagePath + "\x00" + kind + "\x00" + name
		entry := entryByKey[key]
		entry.PackagePath = packagePath
		entry.Kind = kind
		entry.Name = name
		entry.RequiresNetwork = entry.RequiresNetwork || requiresNetwork
		entryByKey[key] = entry
	}
	for _, entry := range entryByKey {
		catalog.entries = append(catalog.entries, entry)
	}
	sort.Slice(catalog.entries, func(left, right int) bool {
		a, b := catalog.entries[left], catalog.entries[right]
		if a.PackagePath != b.PackagePath {
			return a.PackagePath < b.PackagePath
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return catalog, nil
}

func relativePackagePath(root, file string) (string, error) {
	relative, err := filepath.Rel(root, filepath.Dir(file))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

func testingEntryKind(function *ssa.Function) string {
	object := functionObject(function)
	if object == nil || !isTestingEntryName(object.Name()) {
		return ""
	}
	signature := function.Signature
	if signature == nil || signature.Recv() != nil || signature.Params().Len() != 1 ||
		signature.Results().Len() != 0 || signature.Variadic() {
		return ""
	}
	parameter := signature.Params().At(0).Type()
	pointer, ok := parameter.(*types.Pointer)
	if !ok {
		return ""
	}
	named, ok := pointer.Elem().(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil ||
		named.Obj().Pkg().Path() != testingPackagePath {
		return ""
	}
	switch named.Obj().Name() {
	case "T":
		if hasTestingPrefix(object.Name(), "Test") {
			return "test"
		}
	case "B":
		if hasTestingPrefix(object.Name(), "Benchmark") {
			return "benchmark"
		}
	}
	return ""
}

func isTestingLifecycle(function *ssa.Function) bool {
	object := functionObject(function)
	if object == nil {
		// Source and synthetic package initializers have no types.Func. Folding
		// them into every operation prevents selection flags from bypassing work
		// that the Go runtime necessarily performs before the chosen entry.
		return function.Name() == "init" || strings.HasPrefix(function.Name(), "init#")
	}
	if object.Name() != "TestMain" {
		return false
	}
	signature := function.Signature
	if signature == nil || signature.Recv() != nil || signature.Params().Len() != 1 ||
		signature.Results().Len() != 0 || signature.Variadic() {
		return false
	}
	pointer, ok := signature.Params().At(0).Type().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := pointer.Elem().(*types.Named)
	return ok && named.Obj() != nil && named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == testingPackagePath && named.Obj().Name() == "M"
}

func isTestingEntryName(name string) bool {
	return hasTestingPrefix(name, "Test") || hasTestingPrefix(name, "Benchmark")
}

// hasTestingPrefix mirrors cmd/go's Unicode entry-name boundary. The boundary
// keeps Testicular a helper while accepting names whose suffix starts non-lowercase.
func hasTestingPrefix(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(r)
}

func functionReachesRuntimeGate(
	entry *ssa.Function,
	policies map[*ssa.Function]*functionPolicy,
	resolved map[ssa.CallInstruction][]*ssa.Function,
) bool {
	pending := []reachabilityState{{function: entry}}
	visited := make(map[string]bool)
	for len(pending) != 0 {
		state := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		key := reachabilityStateKey(state)
		if visited[key] {
			continue
		}
		visited[key] = true
		policy := policies[state.function]
		if policy == nil {
			continue
		}
		for _, call := range policy.calls {
			targets := stateCallTargets(call, state.bindings, resolved)
			for _, target := range targets {
				if isRuntimeGate(functionObject(target)) {
					return true
				}
				if policies[target] != nil {
					pending = append(pending, bindCallTarget(target, call.Common().Args, state.bindings))
				}
			}
			// Testing callbacks are values consumed by framework calls rather than
			// direct callees. Following their compiler values keeps subtests,
			// cleanups, and benchmark bodies attached to their owning entry.
			for _, argument := range call.Common().Args {
				for _, callback := range resolveFunctionValues(argument, state.bindings, nil) {
					if policies[callback] != nil {
						pending = append(pending, reachabilityState{function: callback})
					}
				}
			}
		}
	}
	return false
}

func stateCallTargets(
	call ssa.CallInstruction,
	bindings map[*ssa.Parameter][]*ssa.Function,
	resolved map[ssa.CallInstruction][]*ssa.Function,
) []*ssa.Function {
	if callee := call.Common().StaticCallee(); callee != nil {
		return []*ssa.Function{callee}
	}
	if targets := resolveFunctionValues(call.Common().Value, bindings, nil); len(targets) != 0 {
		return targets
	}
	return resolved[call]
}

func bindCallTarget(
	target *ssa.Function,
	arguments []ssa.Value,
	callerBindings map[*ssa.Parameter][]*ssa.Function,
) reachabilityState {
	state := reachabilityState{function: target}
	for index := 0; index < len(target.Params) && index < len(arguments); index++ {
		targets := resolveFunctionValues(arguments[index], callerBindings, nil)
		if len(targets) == 0 {
			continue
		}
		if state.bindings == nil {
			state.bindings = make(map[*ssa.Parameter][]*ssa.Function)
		}
		state.bindings[target.Params[index]] = targets
	}
	return state
}

func resolveFunctionValues(
	value ssa.Value,
	bindings map[*ssa.Parameter][]*ssa.Function,
	active map[ssa.Value]bool,
) []*ssa.Function {
	if value == nil {
		return nil
	}
	if parameter, ok := value.(*ssa.Parameter); ok {
		return bindings[parameter]
	}
	if function, ok := value.(*ssa.Function); ok {
		return []*ssa.Function{function}
	}
	if active == nil {
		active = make(map[ssa.Value]bool)
	}
	if active[value] {
		return nil
	}
	active[value] = true
	defer delete(active, value)
	var values []ssa.Value
	switch value := value.(type) {
	case *ssa.MakeClosure:
		values = append(values, value.Fn)
	case *ssa.MakeInterface:
		values = append(values, value.X)
	case *ssa.ChangeInterface:
		values = append(values, value.X)
	case *ssa.ChangeType:
		values = append(values, value.X)
	case *ssa.Convert:
		values = append(values, value.X)
	case *ssa.TypeAssert:
		values = append(values, value.X)
	case *ssa.Extract:
		values = append(values, value.Tuple)
	case *ssa.Phi:
		values = append(values, value.Edges...)
	default:
		return nil
	}
	set := make(map[*ssa.Function]bool)
	for _, nested := range values {
		for _, function := range resolveFunctionValues(nested, bindings, active) {
			set[function] = true
		}
	}
	result := make([]*ssa.Function, 0, len(set))
	for function := range set {
		result = append(result, function)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].String() < result[right].String()
	})
	return result
}

func reachabilityStateKey(state reachabilityState) string {
	var fields []string
	for index, parameter := range state.function.Params {
		targets := state.bindings[parameter]
		if len(targets) == 0 {
			continue
		}
		names := make([]string, 0, len(targets))
		for _, target := range targets {
			names = append(names, target.String())
		}
		sort.Strings(names)
		fields = append(fields, fmt.Sprintf("%d=%s", index, strings.Join(names, ",")))
	}
	return state.function.String() + "|" + strings.Join(fields, ";")
}
