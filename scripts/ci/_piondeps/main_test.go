package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectionRejectsMutationExtraOmissionAndTraversal(t *testing.T) {
	root := t.TempDir()
	raw := []byte("package example\n")
	file := sourceFile{Path: "source.go", SHA256: digest(raw)}
	if err := os.WriteFile(filepath.Join(root, file.Path), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFiles(root, []sourceFile{file}); err != nil {
		t.Fatal(err)
	}
	if err := verifyFiles(root, []sourceFile{{Path: "../source.go", SHA256: file.SHA256}}); err == nil {
		t.Fatal("traversal accepted")
	}
	if err := verifyFiles(root, []sourceFile{file, file}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := verifyFiles(root, []sourceFile{file, {Path: "missing.go", SHA256: file.SHA256}}); err == nil {
		t.Fatal("omitted file accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "extra.go"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFiles(root, []sourceFile{file}); err == nil {
		t.Fatal("unmanifested file accepted")
	}
	if err := os.Remove(filepath.Join(root, "extra.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, file.Path), []byte("mutated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFiles(root, []sourceFile{file}); err == nil {
		t.Fatal("mutated source accepted")
	}
}
func TestLineEndingNormalization(t *testing.T) {
	if got := string(normalize([]byte("a\r\nb\n"))); got != "a\nb\n" {
		t.Fatal(got)
	}
}

func TestModuleBindingsRejectUnselectedOrUnpinnedSources(t *testing.T) {
	module := sourceModule{Name: "ice", Path: "github.com/pion/ice/v4", Version: "v4.2.7"}
	valid := "module example.com/product\nrequire github.com/pion/ice/v4 v4.2.7\nreplace github.com/pion/ice/v4 => ./third_party/pion/ice\n"
	if err := verifyModuleBindings([]byte(valid), []sourceModule{module}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"wrong version":      strings.Replace(valid, "v4.2.7", "v4.2.6", 1),
		"missing require":    strings.Replace(valid, "require github.com/pion/ice/v4 v4.2.7\n", "", 1),
		"missing replace":    strings.Split(valid, "replace")[0],
		"other directory":    strings.Replace(valid, "./third_party/pion/ice", "../ice", 1),
		"versioned override": strings.Replace(valid, "replace github.com/pion/ice/v4", "replace github.com/pion/ice/v4 v4.2.7", 1),
		"remote fork":        strings.Replace(valid, "./third_party/pion/ice", "example.com/ice/v4 v4.2.7", 1),
		"duplicate override": valid + "replace github.com/pion/ice/v4 v4.2.8 => ../ice\n",
		"invalid module":     "invalid syntax",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := verifyModuleBindings([]byte(raw), []sourceModule{module}); err == nil {
				t.Fatal("invalid production source binding accepted")
			}
		})
	}
}
