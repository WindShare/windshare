package main

import (
	"bytes"
	"encoding/json"

	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestMaintainedGoFilesIncludeUntrackedSources(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	write := func(path, content string) {
		t.Helper()
		path = filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--quiet")
	git("config", "core.excludesfile", filepath.Join(root, ".git", "info", "exclude"))
	for _, file := range []string{"tracked.go", "deleted.go", "ignored/tracked.go", "staged.go"} {
		write(file, "package example\n")
	}
	git("add", "--", "*.go")
	write(".gitignore", "ignored/\n")
	write("tracked.go", "package example\n\nconst Updated = true\n")
	write("new.go", "package example\n")
	write("new package/文件_test.go", "package example\n")
	write("ignored/generated.go", "package example\n")
	write("notes.txt", "not Go source\n")
	if err := os.Remove(filepath.Join(root, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	candidates, err := goSourceCandidates(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := maintainedGoFiles(root, candidates, manifest{})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"ignored/tracked.go", "new package/文件_test.go", "new.go", "staged.go", "tracked.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("maintained working-tree sources = %q, want %q", got, want)
	}
}

func TestMaintainedGoFilesExcludeOnlyExactVerifiedProjection(t *testing.T) {
	root := t.TempDir()
	owned := []string{
		"core/session/new_test.go",
		"connectivity/pionpeer/adapter.go",
		"scripts/ci/_piondeps/maintained.go",
		"spikes/webrtc/main.go",
		"third_party/other/adapter.go",
		"third_party/pion/adapter.go",
		"third_party/pion/sctp_extra/adapter.go",
		"third_party/pion/sctp/new_test.go",
		"directory with spaces/source.go",
	}
	for _, file := range owned {
		target := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("package example\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	projection := manifest{Modules: []sourceModule{
		{Name: "sctp", Files: []sourceFile{{Path: "source.go"}}},
	}}
	candidates := append([]string{"third_party/pion/sctp/source.go"}, owned...)
	candidates = append(candidates, "deleted.go", "")
	got, err := maintainedGoFiles(root, candidates, projection)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, owned) {
		t.Fatalf("maintained sources = %q, want %q", got, owned)
	}
	for _, invalid := range []string{"../escape.go", "file.txt"} {
		if _, err := maintainedGoFiles(root, []string{invalid}, projection); err == nil {
			t.Fatalf("invalid source path accepted: %q", invalid)
		}
	}
}

// These failures precede reproduction, Git enumeration, and any output. The
// launcher must never receive an exemption list for an unverified projection.
func TestMaintainedOutputRejectsUnverifiedProjection(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "modified source", true: "malformed manifest"}[malformed], func(t *testing.T) {
			root := t.TempDir()
			boundary := filepath.Join(root, "third_party", "pion")
			if err := os.MkdirAll(filepath.Join(boundary, "sctp"), 0700); err != nil {
				t.Fatal(err)
			}
			sources := manifest{Modules: []sourceModule{
				{Name: "sctp", Path: "github.com/pion/sctp", Version: "v1.12.0", Revision: "0123456789012345678901234567890123456789",
					Files: []sourceFile{{Path: "source.go", SHA256: digest([]byte("package expected\n"))}}},
			}}
			raw, err := json.Marshal(sources)
			if err != nil {
				t.Fatal(err)
			}
			if malformed {
				raw = []byte("{")
			}
			if err := os.WriteFile(filepath.Join(boundary, "manifest.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(boundary, "sctp", "source.go"), []byte("package changed\n"), 0600); err != nil {
				t.Fatal(err)
			}
			module := "module example.com/product\nrequire (\ngithub.com/pion/sctp v1.12.0\n)\nreplace github.com/pion/sctp => ./third_party/pion/sctp\n"
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(module), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := writeDiagnosticPlan(root, &output); err == nil {
				t.Fatal("unverified source selection succeeded")
			}
			if output.Len() != 0 {
				t.Fatalf("failed selection emitted paths: %q", output.String())
			}
		})
	}
}
