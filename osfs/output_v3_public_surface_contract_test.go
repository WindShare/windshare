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
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/outputcap"
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

func TestOutputValuesDoNotLeakAuthorityInternals(t *testing.T) {
	forbidden := []reflect.Type{
		reflect.TypeFor[outputcap.Directory](),
		reflect.TypeFor[outputcap.Platform](),
		reflect.TypeFor[*checkpointstore.OperationRegistry](),
		reflect.TypeFor[*destinationauthority.BoundDestination](),
	}
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
			for _, rejected := range forbidden {
				if field.Type == rejected || field.Type.AssignableTo(rejected) {
					t.Fatalf("%s field %s leaks %s", value, field.Name, rejected)
				}
			}
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
		"FilesystemOutputAuthority":         {},
		"FilesystemOutputCreated":           {},
		"FilesystemOutputFilePhase":         {},
		"FilesystemOutputOpen":              {},
		"FilesystemOutputOpenKind":          {},
		"FilesystemOutputRecoveryAction":    {},
		"FilesystemOutputReopened":          {},
		"FilesystemOutputRequest":           {},
		"FilesystemOutputSession":           {},
		"FilesystemOutputStateInstallStage": {},
		"FilesystemOutputTrace":             {},
		"MaxFilesystemOutputTransactions":   {},
		"NativeDirectTreeReservation":       {},
		"NewFilesystemOutputAuthority":      {},
		"NewFilesystemResumeStateAuthority": {},
		"OpenOrCreate":                      {},
		"OutputObjectIDGenerator":           {},
		"OutputSessionIDGenerator":          {},
		"ResumeStateAuthority":              {},
		"CheckpointIdentityEqual":           {},
		"RecoverFileCheckpoint":             {},
		"SelectVerifiedProgress":            {},
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
		filepath.Join("internal", "incrementaladmission"),
		filepath.Join("internal", "deliveryreceipts"),
	} {
		path := filepath.Join(packageDirectory, relative)
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("retired state package still exists at %s: %v", path, err)
		}
	}
	retiredImports := []string{
		"github.com/windshare/windshare/osfs/resumestate",
		"github.com/windshare/windshare/osfs/internal/resumestate",
		"github.com/windshare/windshare/osfs/internal/outputnamespace",
		"github.com/windshare/windshare/osfs/internal/incrementaladmission",
		"github.com/windshare/windshare/osfs/internal/deliveryreceipts",
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
