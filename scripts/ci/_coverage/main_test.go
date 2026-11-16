package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProfileSourcesOwnsExactPathsWithoutReinterpretingCoverage(t *testing.T) {
	profile := "mode: atomic\nexample.test/root/z/文件 with spaces.go:1.1,2.2 1 0\n" +
		"example.test/root/a.go:1.1,2.2 1 1\nexample.test/root/a.go:3.1,4.2 2 0\n"
	files, err := profileSources(strings.NewReader(profile), "example.test/root")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.go", "z/文件 with spaces.go"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("sources = %q, want %q", files, want)
	}
	for _, profile := range []string{
		"", "mode: invalid\n", "mode: count\ninvalid block\n",
		"mode: set\nexample.test/other/a.go:1.1,2.2 1 1\n",
		"mode: set\nexample.test/root/../escape.go:1.1,2.2 1 1\n",
		"mode: set\nexample.test/root/a/../b.go:1.1,2.2 1 1\n",
		"mode: set\nexample.test/root//absolute.go:1.1,2.2 1 1\n",
	} {
		if _, err := profileSources(strings.NewReader(profile), "example.test/root"); err == nil {
			t.Fatalf("invalid profile accepted: %q", profile)
		}
	}
}

func TestVerdictGetsOnlyProfileSourcesAndAlwaysReleasesThem(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "pass", true: "failure"}[failure], func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"go.mod":                   "module example.test/root\n",
				"a.go":                     "package root\n// coverage-ignore: preserve annotation bytes\n",
				"nested/文件 with spaces.go": "package nested\r\n",
				"cache/unrelated.go":       "this must never enter source discovery",
				".testcoverage.yml":        "threshold:\n  total: 60\n",
				"cover.out": "mode: atomic\nexample.test/root/a.go:1.1,2.2 1 0\n" +
					"example.test/root/nested/文件 with spaces.go:1.1,2.2 1 1\n",
			}
			for name, content := range files {
				writeFixture(t, root, name, content)
			}
			var staged string
			verdictError := errors.New("verdict rejected")
			err := run(context.Background(), root, nil, io.Discard, io.Discard,
				func(_ context.Context, cwd string, args []string, _, _ io.Writer) error {
					if cwd != root || len(args) != 3 {
						t.Fatalf("unexpected invocation: %q %q", cwd, args)
					}
					if args[0] != "--config="+filepath.Join(root, ".testcoverage.yml") ||
						args[1] != "--profile="+filepath.Join(root, "cover.out") {
						t.Fatalf("original policy/profile not forwarded: %q", args)
					}
					staged = strings.TrimPrefix(args[2], "--source-dir=")
					for _, name := range []string{"go.mod", "a.go", "nested/文件 with spaces.go"} {
						data, err := os.ReadFile(filepath.Join(staged, filepath.FromSlash(name)))
						if err != nil || string(data) != files[name] {
							t.Fatalf("source %s changed: %q, %v", name, data, err)
						}
					}
					if _, err := os.Stat(filepath.Join(staged, "cache")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("unrelated cache entered projection: %v", err)
					}
					if failure {
						return verdictError
					}
					return nil
				})
			if failure && !errors.Is(err, verdictError) || !failure && err != nil {
				t.Fatalf("verdict outcome changed: %v", err)
			}
			if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned sources survived verdict: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(root, "a.go"))
			if err != nil || string(data) != files["a.go"] {
				t.Fatalf("original source changed: %q, %v", data, err)
			}
		})
	}
}

func TestMissingProfileSourceCannotMatchAnUnrelatedBasename(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.test/root\n")
	writeFixture(t, root, "cover.out", "mode: count\nexample.test/root/missing/a.go:1.1,2.2 1 1\n")
	writeFixture(t, root, "cache/a.go", "package unrelated\n")
	called := false
	err := run(context.Background(), root, nil, io.Discard, io.Discard,
		func(context.Context, string, []string, io.Writer, io.Writer) error {
			called = true
			return nil
		})
	if err == nil || called {
		t.Fatalf("missing source reached verdict: called=%v, error=%v", called, err)
	}
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
