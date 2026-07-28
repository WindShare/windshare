package osfs

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

var _ transfer.OutputAuthority = (*FilesystemOutputAuthority)(nil)

func TestFilesystemOutputAuthorityExportsOnlyIntentionalSurface(t *testing.T) {
	authorityType := reflect.TypeFor[*FilesystemOutputAuthority]()
	methods := make([]string, 0, authorityType.NumMethod())
	for method := range authorityType.Methods() {
		methods = append(methods, method.Name)
	}
	want := []string{"OpenSelection"}
	if !slices.Equal(methods, want) {
		t.Fatalf("public filesystem output-authority methods = %v, want %v", methods, want)
	}
}

func TestFilesystemOutputAuthorityOpenSelectionReturnsTransferContract(t *testing.T) {
	authorityType := reflect.TypeFor[*FilesystemOutputAuthority]()
	method, found := authorityType.MethodByName("OpenSelection")
	if !found {
		t.Fatal("filesystem output authority does not expose OpenSelection")
	}
	transferSession := reflect.TypeFor[transfer.OutputSession]()
	if method.Type.NumOut() != 2 || method.Type.Out(0) != transferSession ||
		method.Type.Out(1) != reflect.TypeFor[error]() {
		t.Fatalf("OpenSelection results = (%v, %v), want (transfer.OutputSession, error)",
			method.Type.Out(0), method.Type.Out(1))
	}
}

func TestFilesystemOutputAuthorityConfigContainsOnlyPathPolicyAndTracer(t *testing.T) {
	configType := reflect.TypeFor[FilesystemOutputAuthorityConfig]()
	want := []struct {
		name   string
		typeOf reflect.Type
	}{
		{name: "RootPath", typeOf: reflect.TypeFor[string]()},
		{name: "CreateRoot", typeOf: reflect.TypeFor[bool]()},
		{name: "Tracer", typeOf: reflect.TypeFor[FilesystemOutputTracer]()},
	}
	if configType.NumField() != len(want) {
		t.Fatalf("filesystem output-authority config fields = %d, want %d", configType.NumField(), len(want))
	}
	for index, expected := range want {
		field := configType.Field(index)
		if field.Name != expected.name || field.Type != expected.typeOf {
			t.Fatalf("filesystem output-authority config field %d = %s %s, want %s %s",
				index, field.Name, field.Type, expected.name, expected.typeOf)
		}
	}
}

func TestFilesystemOutputForbiddenSurfaceIsAbsent(t *testing.T) {
	forbidden := map[string]struct{}{
		"AdmitSelection":                  {},
		"EnsureDirectory":                 {},
		"ErrLegacyOutputState":            {},
		"ErrOutputFileActive":             {},
		"ErrOutputInspectionLimit":        {},
		"ErrOutputIntentUnsafe":           {},
		"ErrOutputRootUnsafe":             {},
		"ErrOutputSessionActive":          {},
		"ErrOutputSessionClosed":          {},
		"ErrOutputTransactionLimit":       {},
		"ErrReservedOutputPath":           {},
		"ErrUnsupportedOutputVolume":      {},
		"FilesystemOutputCreated":         {},
		"FilesystemOutputOpen":            {},
		"FilesystemOutputOpenKind":        {},
		"FilesystemOutputReopened":        {},
		"FilesystemOutputRequest":         {},
		"FilesystemOutputSession":         {},
		"MaxFilesystemOutputTransactions": {},
		"OpenOrCreate":                    {},
		"OutputObjectIDGenerator":         {},
		"OutputSessionIDGenerator":        {},
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate osfs source directory")
	}
	entries, err := os.ReadDir(filepath.Dir(sourceFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(filepath.Dir(sourceFile), entry.Name())
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", entry.Name(), parseErr)
		}
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if _, rejected := forbidden[declaration.Name.Name]; rejected {
					t.Errorf("forbidden output entry point %s remains in %s", declaration.Name.Name, entry.Name())
				}
			case *ast.GenDecl:
				for _, specification := range declaration.Specs {
					for _, name := range declaredIdentifiers(specification) {
						if _, rejected := forbidden[name]; rejected {
							t.Errorf("forbidden output surface %s remains in %s", name, entry.Name())
						}
					}
				}
			}
		}
	}
}

func TestFilesystemOutputPublicSignaturesDoNotLeakResumeState(t *testing.T) {
	packageDirectory := outputPackageDirectory(t)
	entries, err := os.ReadDir(packageDirectory)
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(packageDirectory, entry.Name())
		file, parseErr := parser.ParseFile(files, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", entry.Name(), parseErr)
		}
		aliases := resumeStateImportAliases(file)
		if len(aliases) == 0 {
			continue
		}
		for _, declaration := range file.Decls {
			for _, exposed := range exportedDeclarationNodes(declaration) {
				if nodeReferencesImport(exposed, aliases) {
					t.Errorf("exported declaration in %s references internal resumestate at %s",
						entry.Name(), files.Position(exposed.Pos()))
				}
			}
		}
	}
}

func TestFilesystemOutputResumeStatePackageIsInternalOnly(t *testing.T) {
	packageDirectory := outputPackageDirectory(t)
	publicPath := filepath.Join(packageDirectory, "resumestate")
	if _, err := os.Stat(publicPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("public resumestate package still exists at %s: %v", publicPath, err)
	}
	internalPath := filepath.Join(packageDirectory, "internal", "resumestate")
	if info, err := os.Stat(internalPath); err != nil || !info.IsDir() {
		t.Fatalf("internal resumestate package = (%v, %v)", info, err)
	}
	oldImport := "github.com/windshare/windshare/core/osfs/" + "resumestate"
	err := filepath.WalkDir(packageDirectory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(contents), strconv.Quote(oldImport)) {
			t.Errorf("stale public resumestate import remains in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func outputPackageDirectory(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate osfs source directory")
	}
	return filepath.Dir(sourceFile)
}

func declaredIdentifiers(specification ast.Spec) []string {
	switch specification := specification.(type) {
	case *ast.TypeSpec:
		return []string{specification.Name.Name}
	case *ast.ValueSpec:
		names := make([]string, len(specification.Names))
		for index, name := range specification.Names {
			names[index] = name.Name
		}
		return names
	}
	return nil
}

func resumeStateImportAliases(file *ast.File) map[string]struct{} {
	aliases := make(map[string]struct{})
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path != "github.com/windshare/windshare/core/osfs/internal/resumestate" {
			continue
		}
		name := "resumestate"
		if imported.Name != nil {
			name = imported.Name.Name
		}
		aliases[name] = struct{}{}
	}
	return aliases
}

func exportedDeclarationNodes(declaration ast.Decl) []ast.Node {
	switch declaration := declaration.(type) {
	case *ast.FuncDecl:
		if declaration.Name.IsExported() &&
			(declaration.Recv == nil || receiverTypeIsExported(declaration.Recv)) {
			return []ast.Node{declaration.Type}
		}
	case *ast.GenDecl:
		var result []ast.Node
		for _, specification := range declaration.Specs {
			switch specification := specification.(type) {
			case *ast.TypeSpec:
				if !specification.Name.IsExported() {
					continue
				}
				switch publicType := specification.Type.(type) {
				case *ast.StructType:
					for _, field := range publicType.Fields.List {
						if len(field.Names) == 0 || slices.ContainsFunc(field.Names, (*ast.Ident).IsExported) {
							result = append(result, field.Type)
						}
					}
				case *ast.InterfaceType:
					for _, field := range publicType.Methods.List {
						if len(field.Names) == 0 || slices.ContainsFunc(field.Names, (*ast.Ident).IsExported) {
							result = append(result, field.Type)
						}
					}
				default:
					result = append(result, specification.Type)
				}
			case *ast.ValueSpec:
				if !slices.ContainsFunc(specification.Names, (*ast.Ident).IsExported) {
					continue
				}
				if specification.Type != nil {
					result = append(result, specification.Type)
				}
				for _, value := range specification.Values {
					result = append(result, value)
				}
			}
		}
		return result
	}
	return nil
}

func receiverTypeIsExported(receiver *ast.FieldList) bool {
	if receiver == nil || len(receiver.List) != 1 {
		return false
	}
	receiverType := receiver.List[0].Type
	for {
		switch typed := receiverType.(type) {
		case *ast.StarExpr:
			receiverType = typed.X
		case *ast.IndexExpr:
			receiverType = typed.X
		case *ast.IndexListExpr:
			receiverType = typed.X
		case *ast.Ident:
			return typed.IsExported()
		default:
			return false
		}
	}
}

func nodeReferencesImport(node ast.Node, aliases map[string]struct{}) bool {
	references := false
	ast.Inspect(node, func(candidate ast.Node) bool {
		selector, ok := candidate.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, found := aliases[identifier.Name]; found {
			references = true
			return false
		}
		return true
	})
	return references
}
