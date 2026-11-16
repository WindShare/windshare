package main

import (
	"go/build"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeDiagnosticFixture(t *testing.T, contents map[string]string) (string, []string) {
	t.Helper()
	root := t.TempDir()
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
	return root, files
}

func TestDiagnosticConfigurationsCoverSiblingFilesAndForeignTaggedSources(t *testing.T) {
	root, files := writeDiagnosticFixture(t, map[string]string{
		"go.mod":                "module example.test/root\n",
		"a/common.go":           "package example\n",
		"a/platform_linux.go":   "package example\n\n// //go:build ignored_after_package\n",
		"a/platform_windows.go": "package example\n",
		"a/default.go":          "//go:build !probe\n\npackage example\n",
		"a/tagged.go":           "//go:build probe\n\npackage example\n",
		"a/tagged_linux.go":     "//go:build linux && probe\n\npackage example\n",
		"a/tagged_windows.go":   "//go:build windows && probe\n\npackage example\n",
		"b/combined.go":         "//go:build alpha && beta\n\npackage b\n",
		"tools/gen.go":          "//go:build ignore\n\npackage main\n",
		"z/文件 with spaces.go":   "package example\n",
		"nested/go.mod":         "module example.test/nested\n",
		"nested/a_linux.go":     "package nested\n",
		"nested/a_windows.go":   "package nested\n",
	})
	ports := []goPort{{"linux", "amd64"}, {"windows", "amd64"}}
	profiles := []diagnosticProfile{{Name: "probe", Tags: []string{"probe"}}, {Name: "combined", Tags: []string{"alpha", "beta"}}}
	for _, platform := range []string{"windows", "linux"} {
		t.Run(platform, func(t *testing.T) {
			host := build.Default
			host.GOOS, host.GOARCH, host.CgoEnabled = platform, "amd64", true
			host.BuildTags = []string{"ambient_tag_must_be_ignored"}
			plan, err := planDiagnosticSources(root, files, host, ports, profiles)
			if err != nil {
				t.Fatal(err)
			}
			opened := make(map[string]bool)
			var taggedPorts []string
			for _, session := range plan {
				for _, file := range session.OpenFiles {
					opened[filepath.ToSlash(filepath.Join(session.Directory, file))] = true
					if !slices.Contains(session.DiagnosticFiles, file) {
						t.Fatalf("opened source missing from diagnostic scope: %+v", session)
					}
				}
				if session.Build.CgoEnabled != (session.Build.GOOS == platform) {
					t.Fatalf("cgo differs from the selected target: %+v", session.Build)
				}
				if session.Profile != "probe" {
					continue
				}
				taggedPorts = append(taggedPorts, session.Build.GOOS)
				for _, file := range []string{"a/common.go", "a/platform_" + session.Build.GOOS + ".go", "a/tagged.go"} {
					if !slices.Contains(session.DiagnosticFiles, file) {
						t.Fatalf("profile omitted active sibling %s: %+v", file, session)
					}
				}
				for _, file := range []string{"a/default.go", "z/文件 with spaces.go", "tools/gen.go"} {
					if slices.Contains(session.DiagnosticFiles, file) {
						t.Fatalf("profile selected inactive or unrelated source %s", file)
					}
				}
			}
			slices.Sort(taggedPorts)
			if !slices.Equal(taggedPorts, []string{"linux", "windows"}) {
				t.Fatalf("tagged target configurations = %v", taggedPorts)
			}
			if len(opened) != len(files) {
				t.Fatalf("opened %d of %d sources", len(opened), len(files))
			}
			slices.Reverse(files)
			reordered, err := planDiagnosticSources(root, files, host, ports, profiles)
			if err != nil || !reflect.DeepEqual(reordered, plan) {
				t.Fatalf("input order changed the plan: %v", err)
			}
		})
	}
}

func TestDiagnosticProfilesAreExplicitRatherThanInferredMinimalTagSets(t *testing.T) {
	root, files := writeDiagnosticFixture(t, map[string]string{
		"go.mod":         "module example.test/profile\n",
		"alternative.go": "//go:build alpha || beta\n\npackage example\n",
	})
	host := build.Default
	ports := []goPort{{host.GOOS, host.GOARCH}}
	if _, err := planDiagnosticSources(root, files, host, ports, nil); err == nil || !strings.Contains(err.Error(), diagnosticProfilesPath) {
		t.Fatalf("unconfigured tags must name the profile configuration: %v", err)
	}
	profiles := []diagnosticProfile{{Name: "alpha", Tags: []string{"alpha"}}, {Name: "beta", Tags: []string{"beta"}}}
	plan, err := planDiagnosticSources(root, files, host, ports, profiles)
	if err != nil || len(plan) != 2 {
		t.Fatalf("both declared alternatives must be checked: %+v, %v", plan, err)
	}
}

func TestDiagnosticPlanRejectsUnreadableAndUnsupportedConstraints(t *testing.T) {
	contents := map[string]string{
		"malformed.go":              "//go:build (\n\npackage example\n",
		"contradictory_platform.go": "//go:build linux && windows\n\npackage example\n",
		"contradictory_custom.go":   "//go:build probe && !probe\n\npackage example\n",
		"ignored_library.go":        "//go:build ignore\n\npackage example\n",
		"ignored_test.go":           "//go:build ignore\n\npackage main\n",
		"_leading_underscore.go":    "package example\n",
	}
	root, _ := writeDiagnosticFixture(t, contents)
	ports := []goPort{{"linux", "amd64"}, {"windows", "amd64"}}
	profiles := []diagnosticProfile{{Name: "probe", Tags: []string{"probe"}}}
	for _, file := range append([]string{"missing.go"}, slices.Sorted(maps.Keys(contents))...) {
		if got, err := planDiagnosticSources(root, []string{file}, build.Default, ports, profiles); err == nil || got != nil {
			t.Fatalf("bad source %s produced a diagnostic plan: %#v, %v", file, got, err)
		}
	}
}

func TestDiagnosticPlanRespectsCgoCapability(t *testing.T) {
	root, files := writeDiagnosticFixture(t, map[string]string{
		"go.mod": "module example.test/cgo\n", "cgo.go": "package example\nimport \"C\"\n",
	})
	host := build.Default
	host.CgoEnabled = false
	ports := []goPort{{host.GOOS, host.GOARCH}}
	if _, err := planDiagnosticSources(root, files, host, ports, nil); err == nil {
		t.Fatal("cgo source accepted with cgo disabled")
	}
	host.CgoEnabled = true
	if _, err := planDiagnosticSources(root, files, host, ports, nil); err != nil {
		t.Fatal(err)
	}
}

func TestReadDiagnosticProfiles(t *testing.T) {
	cases := []struct {
		raw   string
		valid bool
	}{
		{`[{"name":"probe","tags":["probe"]}]`, true},
		{`[{"name":"default","tags":["probe"]}]`, false},
		{`[{"name":"probe","tags":[]}]`, false},
		{`[{"name":"probe","tags":["probe"],"typo":true}]`, false},
		{`[{"name":"probe","tags":["alpha,beta"]}]`, false},
		{`[{"name":"probe","tags":["alpha || beta"]}]`, false},
		{`[{"name":"generator","tags":["ignore"]}]`, false},
	}
	for _, test := range cases {
		root, _ := writeDiagnosticFixture(t, map[string]string{diagnosticProfilesPath: test.raw})
		profiles, err := readDiagnosticProfiles(root)
		if test.valid != (err == nil && len(profiles) == 1) {
			t.Fatalf("profiles %s: %v", test.raw, err)
		}
	}
}

func TestGoDistPortsIncludesCoreTargets(t *testing.T) {
	ports, err := goDistPorts()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []goPort{{"linux", "amd64"}, {"windows", "amd64"}, {"darwin", "amd64"}} {
		if !slices.Contains(ports, want) {
			t.Fatalf("goDistPorts() missing %+v", want)
		}
	}
}
