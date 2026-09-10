package main

import (
	"bytes"
	"encoding/json"
	"go/build"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestMaintainedGoFilesExcludeOnlyExactVerifiedProjection(t *testing.T) {
	root := t.TempDir()
	owned := []string{
		"core/session/new_test.go",
		"transport/webrtc/provider/adapter.go",
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
	}}
	tracked := append([]string{"third_party/pion/ice/source.go", "third_party/pion/webrtc/source.go"}, owned...)
	tracked = append(tracked, "deleted.go", "")
	got, err := maintainedGoFiles(root, tracked, projection)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, owned) {
		t.Fatalf("maintained sources = %q, want %q", got, owned)
	}
	for _, invalid := range []string{"../escape.go", "file.txt"} {
		if _, err := maintainedGoFiles(root, []string{invalid}, projection); err == nil {
			t.Fatalf("invalid tracked path accepted: %q", invalid)
		}
	}
}

func TestDiagnosticOrderDefersForeignViewsWithoutOmittingSources(t *testing.T) {
	root := t.TempDir()
	contents := map[string]string{
		"go.mod":                        "module example.test/root\n",
		"a/platform_linux.go":           "package example\n",
		"a/platform_windows.go":         "package example\n",
		"a/tagged.go":                   "//go:build windshare_probe\n\npackage example\n",
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
	for _, platform := range []string{"windows", "linux"} {
		t.Run(platform, func(t *testing.T) {
			context := build.Default
			context.GOOS, context.GOARCH = platform, "amd64"
			context.BuildTags = nil
			foreign := map[string]string{"windows": "linux", "linux": "windows"}[platform]
			want := []string{
				"a/platform_" + platform + ".go", "z/native_test.go", "z/\u6587\u4ef6 with spaces.go",
				"a/platform_" + foreign + ".go", "a/tagged.go",
				"middle/evidence/a_" + platform + ".go", "middle/evidence/z.go",
				"middle/evidence/a_" + foreign + ".go",
			}
			got, err := orderDiagnosticSources(root, files, context)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("diagnostic order = %q, want %q", got, want)
			}
			slices.Reverse(files)
			reordered, err := orderDiagnosticSources(root, files, context)
			if err != nil || !slices.Equal(reordered, got) {
				t.Fatalf("input order changed diagnostic plan: %q, %v", reordered, err)
			}
		})
	}
}

func TestDiagnosticOrderRejectsUnreadableConstraints(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"missing.go", "malformed.go"} {
		if file == "malformed.go" {
			if err := os.WriteFile(filepath.Join(root, file), []byte("//go:build (\n\npackage example\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := orderDiagnosticSources(root, []string{file}, build.Default); err == nil || got != nil {
			t.Fatalf("bad source produced a diagnostic plan: %q, %v", got, err)
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
			module := "module example.com/product\nrequire (\ngithub.com/pion/ice/v4 v4.2.7\ngithub.com/pion/webrtc/v4 v4.2.16\n)\nreplace github.com/pion/ice/v4 => ./third_party/pion/ice\nreplace github.com/pion/webrtc/v4 => ./third_party/pion/webrtc\n"
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(module), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := writeMaintainedGoFiles(root, false, &output); err == nil {
				t.Fatal("unverified source selection succeeded")
			}
			if output.Len() != 0 {
				t.Fatalf("failed selection emitted paths: %q", output.String())
			}
		})
	}
}
