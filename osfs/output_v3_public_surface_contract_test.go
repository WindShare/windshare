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
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

var _ transfer.DirectTreeMaterializer = (*OutputOperation)(nil)

func TestOutputAuthorityExportsOnlyIntentionalSurface(t *testing.T) {
	authorityType := reflect.TypeFor[*OutputAuthority]()
	methods := make([]string, 0, authorityType.NumMethod())
	for method := range authorityType.Methods() {
		methods = append(methods, method.Name)
	}
	if want := []string{"BindDestination", "Close", "SelectOperation"}; !slices.Equal(methods, want) {
		t.Fatalf("public output-authority methods = %v, want %v", methods, want)
	}

	recoveryType := reflect.TypeFor[*RecoveryAuthority]()
	recoveryMethods := make([]string, 0, recoveryType.NumMethod())
	for method := range recoveryType.Methods() {
		recoveryMethods = append(recoveryMethods, method.Name)
	}
	if want := []string{"Discard", "List"}; !slices.Equal(recoveryMethods, want) {
		t.Fatalf("public recovery-authority methods = %v, want %v", recoveryMethods, want)
	}
}

func TestOutputValuesDoNotExportInternalFields(t *testing.T) {
	for _, value := range []reflect.Type{
		reflect.TypeFor[OutputAuthority](),
		reflect.TypeFor[OutputLookup](),
		reflect.TypeFor[OutputOperation](),
		reflect.TypeFor[RecoveryAuthority](),
	} {
		for field := range value.Fields() {
			if field.IsExported() {
				t.Fatalf("%s exports field %s", value, field.Name)
			}
		}
	}
}

func TestFilesystemOutputForbiddenSurfaceIsAbsent(t *testing.T) {
	forbidden := map[string]struct{}{
		"AdmitSelection":                    {},
		"EnsureDirectory":                   {},
		"CompatibleNameReport":              {},
		"CheckpointReconciliationStep":      {},
		"FilesystemOutputAuthority":         {},
		"NewFilesystemOutputAuthority":      {},
		"NewFilesystemResumeStateAuthority": {},
		"ResumeStateAuthority":              {},
	}
	packageDirectory := outputPackageDirectory(t)
	entries, err := os.ReadDir(packageDirectory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(packageDirectory, entry.Name())
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

func TestFilesystemOutputRetiredStatePackagesAreAbsent(t *testing.T) {
	packageDirectory := outputPackageDirectory(t)
	retired := []string{
		"resumestate",
		filepath.Join("internal", "checkpointmodel"),
		filepath.Join("internal", "checkpointstore"),
		filepath.Join("internal", "destinationauthority"),
		filepath.Join("internal", "directoryauthority"),
		filepath.Join("internal", "fileexecution"),
		filepath.Join("internal", "namestore"),
		filepath.Join("internal", "outputcap"),
		filepath.Join("internal", "outputfault"),
		filepath.Join("internal", "outputnames"),
		filepath.Join("internal", "outputportable"),
		filepath.Join("internal", "outputprobe"),
		filepath.Join("internal", "outputruntime"),
		filepath.Join("internal", "outputsession"),
		filepath.Join("internal", "recoverywitness"),
		filepath.Join("internal", "resumeauthority"),
	}
	for _, relative := range retired {
		path := filepath.Join(packageDirectory, relative)
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("retired state package still exists at %s: %v", path, err)
		}
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
	default:
		return nil
	}
}
