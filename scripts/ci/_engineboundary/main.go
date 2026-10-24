// Command engineboundary checks application imports and exported contracts.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const module = "github.com/windshare/windshare"

func prohibited(path string) bool {
	for _, family := range []string{"/connectivity", "/transport", "/relay", "/osfs", "/core/osfs", "/adapters"} {
		if path == module+family || strings.HasPrefix(path, module+family+"/") {
			return true
		}
	}
	return false
}

func storageImplementation(path string) bool {
	for _, family := range []string{"/osfs", "/core/osfs", "/adapters"} {
		if path == module+family || strings.HasPrefix(path, module+family+"/") {
			return true
		}
	}
	return false
}
func prohibitedAPI(path string) bool {
	if !strings.HasPrefix(path, module+"/") {
		return false
	}
	// Process-test milestones are value-only contracts, still traversed below.
	if path == module+"/internal/testrun" {
		return false
	}
	for _, contract := range []string{"/core", "/engine"} {
		if path == module+contract || strings.HasPrefix(path, module+contract+"/") {
			return false
		}
	}
	return true
}

// Parse all production files, including inactive platform variants, so a local
// platform cannot conceal a forbidden import until another CI runner builds it.
func sourceFindings(root string) ([]string, error) {
	var findings []string
	for _, subtree := range []string{"cmd", "engine"} {
		err := filepath.WalkDir(filepath.Join(root, subtree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				dependency, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				relative, _ := filepath.Rel(root, path)
				application := strings.HasPrefix(filepath.ToSlash(relative), "engine/")
				observation := strings.HasPrefix(filepath.ToSlash(relative), "engine/observation/")
				forbidden := storageImplementation(dependency) && application ||
					prohibited(dependency) && (observation || !application && !storageImplementation(dependency))
				if forbidden {
					relative, _ := filepath.Rel(root, path)
					findings = append(findings, fmt.Sprintf("%s imports implementation package %s", relative, dependency))
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return findings, nil
}

// Alias resolution and recursive public-field/method traversal prevent a facade
// alias or a factory return value from smuggling native ownership into the API.
func typeFindings(label string, root types.Type) []string {
	seen := map[types.Type]bool{}
	var findings []string
	var walk func(string, types.Type)
	walk = func(at string, typ types.Type) {
		if typ == nil || seen[typ] {
			return
		}
		seen[typ] = true
		switch value := typ.(type) {
		case *types.Alias:
			walk(at, types.Unalias(value))
		case *types.Named:
			// Even a standard-library container can expose a native type argument.
			for i := 0; i < value.TypeArgs().Len(); i++ {
				walk(at, value.TypeArgs().At(i))
			}
			for i := 0; i < value.TypeParams().Len(); i++ {
				walk(at, value.TypeParams().At(i).Constraint())
			}
			object := value.Obj()
			if object.Pkg() != nil {
				path := object.Pkg().Path()
				if prohibited(path) || prohibitedAPI(path) {
					findings = append(findings, at+" exposes "+path+"."+object.Name())
					return
				}
				// Repository-owned contracts are traversed even when wrapped by core.
				// Standard-library interfaces remain independently owned boundaries.
				if !strings.HasPrefix(path, module+"/") {
					return
				}
			}
			walk(at, value.Underlying())
			for i := 0; i < value.NumMethods(); i++ {
				m := value.Method(i)
				if m.Exported() {
					walk(at+"."+m.Name(), m.Type())
				}
			}
		case *types.Pointer:
			walk(at, value.Elem())
		case *types.Slice:
			walk(at, value.Elem())
		case *types.Array:
			walk(at, value.Elem())
		case *types.Chan:
			walk(at, value.Elem())
		case *types.Map:
			walk(at, value.Key())
			walk(at, value.Elem())
		case *types.Signature:
			walk(at, value.Params())
			walk(at, value.Results())
			for i := 0; i < value.TypeParams().Len(); i++ {
				walk(at, value.TypeParams().At(i).Constraint())
			}
		case *types.Tuple:
			for i := 0; i < value.Len(); i++ {
				walk(at, value.At(i).Type())
			}
		case *types.Struct:
			for i := 0; i < value.NumFields(); i++ {
				f := value.Field(i)
				// Unexported embedded types still promote public fields and methods.
				if f.Exported() || f.Embedded() {
					walk(at+"."+f.Name(), f.Type())
				}
			}
		case *types.Interface:
			for i := 0; i < value.NumMethods(); i++ {
				m := value.Method(i)
				if m.Exported() {
					walk(at+"."+m.Name(), m.Type())
				}
			}
			for i := 0; i < value.NumEmbeddeds(); i++ {
				walk(at, value.EmbeddedType(i))
			}
		case *types.Union:
			for i := 0; i < value.Len(); i++ {
				walk(at, value.Term(i).Type())
			}
		}
	}
	walk(label, root)
	return findings
}

func apiFindings(root string) ([]string, error) {
	executable := os.Getenv("WINDSHARE_GO_EXECUTABLE")
	if executable == "" {
		executable = "go"
	}
	args := []string{"list", "-deps", "-export", "-json", "./engine/..."}
	cmd := exec.Command(executable, args...)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("load application exports: %w: %s", err, &stderr)
	}
	exports := map[string]string{}
	var apiPackages []string
	decoder := json.NewDecoder(&stdout)
	for {
		var pkg struct{ ImportPath, Export string }
		err := decoder.Decode(&pkg)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		exports[pkg.ImportPath] = pkg.Export
		if (pkg.ImportPath == module+"/engine" || strings.HasPrefix(pkg.ImportPath, module+"/engine/")) && !strings.Contains(pkg.ImportPath, "/internal/") {
			apiPackages = append(apiPackages, pkg.ImportPath)
		}
	}
	reader := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, fmt.Errorf("missing export for %s", path)
		}
		return os.Open(file)
	})
	var findings []string
	for _, path := range apiPackages {
		pkg, err := reader.Import(path)
		if err != nil {
			return nil, err
		}
		for _, name := range pkg.Scope().Names() {
			if ast.IsExported(name) {
				findings = append(findings, typeFindings(path+"."+name, pkg.Scope().Lookup(name).Type())...)
			}
		}
	}
	return findings, nil
}
func run(root string) error {
	findings, err := sourceFindings(root)
	if err != nil {
		return err
	}
	exposed, err := apiFindings(root)
	if err != nil {
		return err
	}
	findings = append(findings, exposed...)
	sort.Strings(findings)
	if len(findings) > 0 {
		return fmt.Errorf("%s", strings.Join(findings, "\n"))
	}
	return nil
}
func main() {
	root, err := os.Getwd()
	if err == nil {
		err = run(root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "engine-boundary:", err)
		os.Exit(1)
	}
	fmt.Println("engine-boundary: PASS (cmd imports and exported application contracts)")
}
