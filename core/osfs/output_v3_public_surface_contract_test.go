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

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

var _ transfer.DirectTreeMaterializer = (*FilesystemOutputAuthority)(nil)

func TestFilesystemOutputAuthorityExportsOnlyIntentionalSurface(t *testing.T) {
	authorityType := reflect.TypeFor[*FilesystemOutputAuthority]()
	methods := make([]string, 0, authorityType.NumMethod())
	for method := range authorityType.Methods() {
		methods = append(methods, method.Name)
	}
	if want := []string{"BindDestination", "Close", "CreateOperation", "LookupActive", "OpenDirectTree", "OpenOperation", "ReserveDirectTree"}; !slices.Equal(methods, want) {
		t.Fatalf("public filesystem output-authority methods = %v, want %v", methods, want)
	}

	method, found := authorityType.MethodByName("OpenDirectTree")
	if !found || method.Type.NumOut() != 2 ||
		method.Type.Out(0) != reflect.TypeFor[transfer.DirectTreeSession]() ||
		method.Type.Out(1) != reflect.TypeFor[error]() {
		t.Fatalf("OpenDirectTree signature = %v", method.Type)
	}
	reservation, found := authorityType.MethodByName("ReserveDirectTree")
	if !found || reservation.Type.NumOut() != 2 ||
		reservation.Type.Out(0) != reflect.TypeFor[NativeDirectTreeReservation]() ||
		reservation.Type.Out(1) != reflect.TypeFor[error]() {
		t.Fatalf("ReserveDirectTree signature = %v", reservation.Type)
	}

	stagedSignatures := map[string]struct {
		inputs  int
		outputs []reflect.Type
	}{
		"BindDestination": {inputs: 2, outputs: []reflect.Type{
			reflect.TypeFor[FilesystemOutputExecutionMode](), reflect.TypeFor[error](),
		}},
		"LookupActive": {inputs: 3, outputs: []reflect.Type{
			reflect.TypeFor[FilesystemOutputLookup](), reflect.TypeFor[error](),
		}},
		"CreateOperation": {inputs: 4, outputs: []reflect.Type{
			reflect.TypeFor[FilesystemOutputOperation](), reflect.TypeFor[error](),
		}},
		"OpenOperation": {inputs: 3, outputs: []reflect.Type{
			reflect.TypeFor[transfer.DirectTreeSession](), reflect.TypeFor[error](),
		}},
		"Close": {inputs: 1, outputs: []reflect.Type{reflect.TypeFor[error]()}},
	}
	for name, expected := range stagedSignatures {
		method, found := authorityType.MethodByName(name)
		if !found || method.Type.NumIn() != expected.inputs || method.Type.NumOut() != len(expected.outputs) {
			t.Fatalf("%s signature = %v", name, method.Type)
		}
		for index, output := range expected.outputs {
			if method.Type.Out(index) != output {
				t.Fatalf("%s output %d = %v, want %v", name, index, method.Type.Out(index), output)
			}
		}
	}
}

func TestFilesystemOutputStagedValuesDoNotLeakAuthorityInternals(t *testing.T) {
	for _, value := range []reflect.Type{
		reflect.TypeFor[FilesystemOutputExecutionMode](),
		reflect.TypeFor[FilesystemOutputLookup](),
		reflect.TypeFor[FilesystemOutputOperation](),
	} {
		for field := range value.Fields() {
			if field.IsExported() {
				t.Fatalf("%s exports field %s", value, field.Name)
			}
			forbidden := []reflect.Type{
				reflect.TypeFor[outputcap.Directory](),
				reflect.TypeFor[outputcap.Platform](),
				reflect.TypeFor[*checkpointstore.OperationRegistry](),
				reflect.TypeFor[*destinationauthority.BoundDestination](),
			}
			for _, rejected := range forbidden {
				if field.Type == rejected || field.Type.AssignableTo(rejected) {
					t.Fatalf("%s field %s leaks %s", value, field.Name, rejected)
				}
			}
		}
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
		"AdmitSelection":                    {},
		"EnsureDirectory":                   {},
		"ErrLegacyOutputState":              {},
		"ErrOutputFileActive":               {},
		"ErrOutputInspectionLimit":          {},
		"ErrOutputIntentUnsafe":             {},
		"ErrOutputRootUnsafe":               {},
		"ErrOutputSessionActive":            {},
		"ErrOutputSessionClosed":            {},
		"ErrOutputTransactionLimit":         {},
		"ErrReservedOutputPath":             {},
		"ErrUnsupportedOutputVolume":        {},
		"FilesystemOutputCreated":           {},
		"FilesystemOutputFilePhase":         {},
		"FilesystemOutputOpen":              {},
		"FilesystemOutputOpenKind":          {},
		"FilesystemOutputRecoveryAction":    {},
		"FilesystemOutputReopened":          {},
		"FilesystemOutputRequest":           {},
		"FilesystemOutputSession":           {},
		"FilesystemOutputStateInstallStage": {},
		"MaxFilesystemOutputTransactions":   {},
		"OpenOrCreate":                      {},
		"OutputObjectIDGenerator":           {},
		"OutputSessionIDGenerator":          {},
		"CheckpointIdentityEqual":           {},
		"RecoverFileCheckpoint":             {},
		"SelectVerifiedCheckpoint":          {},
		"ValidateCheckpointTransition":      {},
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
	for _, relative := range []string{
		"resumestate",
		filepath.Join("internal", "resumestate"),
		filepath.Join("internal", "outputnamespace"),
	} {
		path := filepath.Join(packageDirectory, relative)
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("retired state package still exists at %s: %v", path, err)
		}
	}
	retiredImports := []string{
		"github.com/windshare/windshare/core/osfs/resumestate",
		"github.com/windshare/windshare/core/osfs/internal/resumestate",
		"github.com/windshare/windshare/core/osfs/internal/outputnamespace",
	}
	err := filepath.WalkDir(packageDirectory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, retiredImport := range retiredImports {
			if strings.Contains(string(contents), retiredImport) {
				t.Errorf("retired state import %q remains in %s", retiredImport, path)
			}
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
	default:
		return nil
	}
}
