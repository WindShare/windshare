// piondeps verifies the complete pinned source projection and replays the narrow
// patches in an isolated directory. It never writes to the Go module cache.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

type sourceFile struct {
	Path           string `json:"path"`
	UpstreamSHA256 string `json:"upstream_sha256,omitempty"`
	SHA256         string `json:"sha256"`
}
type sourceModule struct {
	Name     string       `json:"name"`
	Path     string       `json:"path"`
	Version  string       `json:"version"`
	Revision string       `json:"revision"`
	Sum      string       `json:"sum"`
	Files    []sourceFile `json:"files"`
}
type manifest struct {
	Modules []sourceModule `json:"modules"`
}

func main() {
	root := flag.String("root", ".", "repository root")
	reproduce := flag.Bool("reproduce", false, "replay patches against checksum-verified upstream modules")
	maintained := flag.Bool("maintained-go-files", false, "list tracked Go sources after verifying and reproducing dependency exclusions")
	nul := flag.Bool("0", false, "separate maintained Go paths with NUL instead of newline")
	flag.Parse()
	if *maintained {
		if err := writeMaintainedGoFiles(*root, *nul, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := verify(*root, *reproduce); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("pinned Pion source projection: verified")
}
func verify(root string, reproduce bool) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	boundary := filepath.Join(root, "third_party", "pion")
	raw, err := os.ReadFile(filepath.Join(boundary, "manifest.json"))
	if err != nil {
		return err
	}
	var sources manifest
	if err = json.Unmarshal(raw, &sources); err != nil {
		return err
	}
	if len(sources.Modules) != 2 {
		return errors.New("provider source manifest must contain ice and webrtc")
	}
	moduleFile, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	if err = verifyModuleBindings(moduleFile, sources.Modules); err != nil {
		return err
	}
	names := make(map[string]bool)
	for _, module := range sources.Modules {
		if (module.Name != "ice" && module.Name != "webrtc") || names[module.Name] || module.Path != "github.com/pion/"+module.Name+"/v4" {
			return errors.New("invalid provider module identity")
		}
		names[module.Name] = true
		if len(module.Revision) != 40 || len(module.Files) == 0 {
			return errors.New("missing pinned provider provenance")
		}
		if err = verifyFiles(filepath.Join(boundary, module.Name), module.Files); err != nil {
			return err
		}
		if reproduce {
			if err = replay(boundary, module); err != nil {
				return err
			}
		}
	}
	return nil
}
func verifyModuleBindings(raw []byte, modules []sourceModule) error {
	parsed, err := modfile.Parse("go.mod", raw, nil)
	if err != nil {
		return err
	}
	// Source verification must also prove that the production build selects it.
	for _, module := range modules {
		requires, replaces := 0, 0
		for _, requirement := range parsed.Require {
			if requirement.Mod.Path == module.Path {
				requires++
				if requirement.Mod.Version != module.Version {
					return fmt.Errorf("provider requirement differs from pinned version: %s", module.Path)
				}
			}
		}
		for _, replacement := range parsed.Replace {
			if replacement.Old.Path == module.Path {
				replaces++
				if replacement.Old.Version != "" || replacement.New.Version != "" || replacement.New.Path != "./third_party/pion/"+module.Name {
					return fmt.Errorf("provider replacement differs from pinned source: %s", module.Path)
				}
			}
		}
		if requires != 1 || replaces != 1 {
			return fmt.Errorf("provider needs one pinned requirement and local replacement: %s", module.Path)
		}
	}
	return nil
}
func safeRelative(value string) bool {
	return value != "" && !strings.Contains(value, "\\") && filepath.IsLocal(value) && filepath.ToSlash(filepath.Clean(value)) == value
}
func digest(raw []byte) string { value := sha256.Sum256(raw); return hex.EncodeToString(value[:]) }
func verifyFiles(root string, files []sourceFile) error {
	expected := make(map[string]string, len(files))
	patched := make(map[string]bool, len(files))
	for _, file := range files {
		if !safeRelative(file.Path) || len(file.SHA256) != 64 || expected[file.Path] != "" {
			return fmt.Errorf("invalid provider file manifest entry: %q", file.Path)
		}
		expected[file.Path] = file.SHA256
		patched[file.Path] = file.SHA256 != file.UpstreamSHA256 && strings.HasSuffix(file.Path, ".go")
	}
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("provider symlink forbidden: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		want, ok := expected[filepath.ToSlash(relative)]
		if !ok {
			return fmt.Errorf("unmanifested provider source: %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Git checkout line endings are not part of the upstream source semantics.
		raw = normalize(raw)
		if digest(raw) != want {
			return fmt.Errorf("provider source differs from pinned patch projection: %s", path)
		}
		// Keep authored patch files formatted without reformatting untouched upstream.
		if patched[filepath.ToSlash(relative)] {
			formatted, formatErr := format.Source(raw)
			if formatErr != nil || !bytes.Equal(formatted, raw) {
				return fmt.Errorf("provider patch source needs gofmt: %s", path)
			}
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if count != len(expected) {
		return fmt.Errorf("provider source projection is incomplete: %s", root)
	}
	return nil
}
func normalize(raw []byte) []byte { return []byte(strings.ReplaceAll(string(raw), "\r\n", "\n")) }
func replay(boundary string, module sourceModule) error {
	command := exec.Command("go", "mod", "download", "-json", module.Path+"@"+module.Version)
	command.Dir = boundary
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("obtain pinned %s: %w", module.Name, err)
	}
	var downloaded struct {
		Dir, Sum string
		Origin   struct{ Hash string }
	}
	if err = json.Unmarshal(output, &downloaded); err != nil {
		return err
	}
	if downloaded.Sum != module.Sum || (downloaded.Origin.Hash != "" && downloaded.Origin.Hash != module.Revision) {
		return fmt.Errorf("upstream provenance mismatch for %s", module.Name)
	}
	stage, err := os.MkdirTemp("", "windshare-pion-replay-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, file := range module.Files {
		if file.UpstreamSHA256 == "" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(downloaded.Dir, filepath.FromSlash(file.Path)))
		if readErr != nil {
			return readErr
		}
		if digest(raw) != file.UpstreamSHA256 {
			return fmt.Errorf("upstream source checksum mismatch: %s", file.Path)
		}
		target := filepath.Join(stage, filepath.FromSlash(file.Path))
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err = os.WriteFile(target, normalize(raw), 0644); err != nil {
			return err
		}
	}
	patch := filepath.Join(boundary, "patches", module.Name+".patch")
	command = exec.Command("git", "apply", "--whitespace=error", patch)
	command.Dir = stage
	if output, err = command.CombinedOutput(); err != nil {
		return fmt.Errorf("replay %s patch: %w: %s", module.Name, err, output)
	}
	return verifyFiles(stage, module.Files)
}
