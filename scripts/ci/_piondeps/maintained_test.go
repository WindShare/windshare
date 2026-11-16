package main

import (
	"bytes"
	"encoding/json"
	"go/build"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
		"third_party/pion/ice_extra/adapter.go",
		"third_party/pion/ice/new_test.go",
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
		{Name: "ice", Files: []sourceFile{{Path: "source.go"}}},
		{Name: "webrtc", Files: []sourceFile{{Path: "source.go"}}},
		{Name: "turn", Files: []sourceFile{{Path: "source.go"}}},
	}}
	candidates := append([]string{"third_party/pion/ice/source.go", "third_party/pion/webrtc/source.go", "third_party/pion/turn/source.go"}, owned...)
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

func TestDiagnosticOrderDefersForeignViewsAndIsolatesTaggedSessions(t *testing.T) {
	root := t.TempDir()
	contents := map[string]string{
		"go.mod":                        "module example.test/root\n",
		"a/platform_linux.go":           "package example\n\n// //go:build ignored_after_package\n",
		"a/platform_windows.go":         "package example\n",
		"a/default_profile.go":          "//go:build !windshare_probe\n\npackage example\n",
		"a/tagged.go":                   "//go:build windshare_probe\n\npackage example\n",
		"a/tagged_linux.go":             "//go:build linux && windshare_probe\n\npackage example\n",
		"a/tagged_windows.go":           "//go:build windows && windshare_probe\n\npackage example\n",
		"a/disjunctive.go":              "//go:build (beta || alpha) && !gamma\n\npackage example\n",
		"b/race_test.go":                "//go:build race\n\npackage b_test\n",
		"tools/gen.go":                  "//go:build ignore\n\npackage main\n",
		"z/native_test.go":              "package example\n",
		"z/\u6587\u4ef6 with spaces.go": "package example\n",
		"middle/evidence/go.mod":        "module example.test/nested\n",
		"middle/evidence/a_linux.go":    "package evidence\n",
		"middle/evidence/a_windows.go":  "package evidence\n",
		"middle/evidence/z.go":          "package evidence\n",
	}
	var files []string
	for file, content := range contents {
		path := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(file, ".go") {
			files = append(files, file)
		}
	}
	ports := []goPort{{goos: "linux", goarch: "amd64"}, {goos: "windows", goarch: "amd64"}}
	for _, platform := range []string{"windows", "linux"} {
		t.Run(platform, func(t *testing.T) {
			context := build.Default
			context.GOOS, context.GOARCH = platform, "amd64"
			context.BuildTags = []string{"ambient_tag_must_be_ignored"}
			foreign := map[string]string{"windows": "linux", "linux": "windows"}[platform]
			want := []diagnosticSession{
				{Directory: ".", Files: []string{
					"a/default_profile.go",
					"a/platform_" + platform + ".go",
					"z/native_test.go",
					"z/\u6587\u4ef6 with spaces.go",
					"a/platform_" + foreign + ".go",
					"tools/gen.go",
				}},
				{Directory: ".", BuildTags: []string{"alpha"}, Files: []string{"a/disjunctive.go"}},
				{Directory: ".", BuildTags: []string{"race"}, Files: []string{"b/race_test.go"}},
				{Directory: ".", BuildTags: []string{"windshare_probe"}, Files: []string{
					"a/tagged.go",
					"a/tagged_" + platform + ".go",
					"a/tagged_" + foreign + ".go",
				}},
				{Directory: "middle/evidence", Files: []string{
					"a_" + platform + ".go", "z.go", "a_" + foreign + ".go",
				}},
			}
			got, err := planDiagnosticSources(root, files, context, ports)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("diagnostic plan = %#v, want %#v", got, want)
			}
			slices.Reverse(files)
			reordered, err := planDiagnosticSources(root, files, context, ports)
			if err != nil || !reflect.DeepEqual(reordered, got) {
				t.Fatalf("input order changed diagnostic plan: %#v, %v", reordered, err)
			}
		})
	}
}

func TestDiagnosticOrderRejectsUnreadableAndUnanalyzableConstraints(t *testing.T) {
	root := t.TempDir()
	cases := map[string]string{
		"malformed.go":              "//go:build (\n\npackage example\n",
		"contradictory_platform.go": "//go:build linux && windows\n\npackage example\n",
		"contradictory_custom.go":   "//go:build probe && !probe\n\npackage example\n",
		"ignored_library.go":        "//go:build ignore\n\npackage example\n",
		"ignored_test.go":           "//go:build ignore\n\npackage main\n",
		"_leading_underscore.go":    "package example\n",
	}
	ports := []goPort{{goos: "linux", goarch: "amd64"}, {goos: "windows", goarch: "amd64"}}
	for file, content := range cases {
		if err := os.WriteFile(filepath.Join(root, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range append([]string{"missing.go"}, slices.Sorted(maps.Keys(cases))...) {
		if got, err := planDiagnosticSources(root, []string{file}, build.Default, ports); err == nil || got != nil {
			t.Fatalf("bad source %s produced a diagnostic plan: %#v, %v", file, got, err)
		}
	}
}

func TestGoDistPortsIncludesCoreTargets(t *testing.T) {
	ports, err := goDistPorts()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []goPort{
		{goos: "linux", goarch: "amd64"},
		{goos: "windows", goarch: "amd64"},
		{goos: "darwin", goarch: "amd64"},
	} {
		if !slices.Contains(ports, want) {
			t.Fatalf("goDistPorts() missing %+v", want)
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
			if err := os.MkdirAll(filepath.Join(boundary, "ice"), 0700); err != nil {
				t.Fatal(err)
			}
			sources := manifest{Modules: []sourceModule{
				{Name: "ice", Path: "github.com/pion/ice/v4", Version: "v4.2.7", Revision: "0123456789012345678901234567890123456789",
					Files: []sourceFile{{Path: "source.go", SHA256: digest([]byte("package expected\n"))}}},
				{Name: "webrtc", Path: "github.com/pion/webrtc/v4", Version: "v4.2.16"},
				{Name: "turn", Path: "github.com/pion/turn/v5", Version: "v5.1.0"},
				{Name: "sctp", Path: "github.com/pion/sctp", Version: "v1.11.1"},
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
			if err := os.WriteFile(filepath.Join(boundary, "ice", "source.go"), []byte("package changed\n"), 0600); err != nil {
				t.Fatal(err)
			}
			module := "module example.com/product\nrequire (\ngithub.com/pion/ice/v4 v4.2.7\ngithub.com/pion/webrtc/v4 v4.2.16\ngithub.com/pion/turn/v5 v5.1.0\ngithub.com/pion/sctp v1.11.1\n)\nreplace github.com/pion/ice/v4 => ./third_party/pion/ice\nreplace github.com/pion/webrtc/v4 => ./third_party/pion/webrtc\nreplace github.com/pion/turn/v5 => ./third_party/pion/turn\nreplace github.com/pion/sctp => ./third_party/pion/sctp\n"
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
