package main

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicAliasesFactoriesAndNestedMethodsCannotHideNativeTypes(t *testing.T) {
	native := types.NewPackage(module+"/connectivity/example", "example")
	concrete := types.NewNamed(types.NewTypeName(token.NoPos, native, "Connection", nil), types.NewStruct(nil, nil), nil)
	api := types.NewPackage(module+"/engine", "engine")
	alias := types.NewAlias(types.NewTypeName(token.NoPos, api, "Connection", nil), types.NewPointer(concrete))
	result := types.NewTuple(types.NewVar(token.NoPos, nil, "", alias))
	factory := types.NewSignatureType(nil, nil, nil, nil, result, false)
	contract := types.NewInterfaceType([]*types.Func{types.NewFunc(token.NoPos, api, "Open", factory)}, nil).Complete()
	nested := types.NewStruct([]*types.Var{types.NewField(token.NoPos, api, "Factory", contract, false)}, nil)
	hidden := types.NewNamed(types.NewTypeName(token.NoPos, api, "adapter", nil), types.NewStruct(nil, nil), nil)
	hidden.AddMethod(types.NewFunc(token.NoPos, api, "Open", types.NewSignatureType(types.NewVar(token.NoPos, api, "receiver", hidden), nil, nil, nil, result, false)))
	embedded := types.NewStruct([]*types.Var{types.NewField(token.NoPos, api, "adapter", hidden, true)}, nil)
	foreign := types.NewPackage("sync/atomic", "atomic")
	container := types.NewNamed(types.NewTypeName(token.NoPos, foreign, "Pointer", nil), types.NewStruct(nil, nil), nil)
	container.SetTypeParams([]*types.TypeParam{types.NewTypeParam(types.NewTypeName(token.NoPos, foreign, "T", nil), types.NewInterfaceType(nil, nil).Complete())})
	instantiated, err := types.Instantiate(nil, container, []types.Type{concrete}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []types.Type{alias, factory, nested, types.NewSlice(alias), embedded, instantiated} {
		findings := typeFindings("Config", typ)
		if len(findings) != 1 || !strings.Contains(findings[0], "example.Connection") {
			t.Fatalf("missed exposed native capability: %v", findings)
		}
	}
	// Private implementation fields do not form the public API.
	private := types.NewStruct([]*types.Var{types.NewField(token.NoPos, api, "connection", alias, false)}, nil)
	if findings := typeFindings("Config", private); len(findings) != 0 {
		t.Fatal(findings)
	}
}
func TestSourceGateChecksInactivePlatformsAndLeavesTestsAtTheirOwnedBoundary(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"cmd/tool", "engine/observation"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"cmd/tool/bad_plan9.go":       "//go:build plan9\n\npackage main\nimport _ \"" + module + "/transport/relayv2\"\n",
		"cmd/tool/fixture_test.go":    "package main\nimport _ \"" + module + "/connectivity/example\"\n",
		"engine/observation/event.go": "package observation\nimport _ \"" + module + "/relay/protocol/v2\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	findings, err := sourceFindings(root)
	if err != nil || len(findings) != 2 {
		t.Fatalf("findings=%v err=%v", findings, err)
	}
}
