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
	module := sourceModule{Name: "sctp", Path: "github.com/pion/sctp", Version: "v1.12.0"}
	valid := "module example.com/product\nrequire github.com/pion/sctp v1.12.0\nreplace github.com/pion/sctp => ./third_party/pion/sctp\n"
	if err := verifyModuleBindings([]byte(valid), []sourceModule{module}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"wrong version":      strings.Replace(valid, "v1.12.0", "v1.11.1", 1),
		"missing require":    strings.Replace(valid, "require github.com/pion/sctp v1.12.0\n", "", 1),
		"missing replace":    strings.Split(valid, "replace")[0],
		"other directory":    strings.Replace(valid, "./third_party/pion/sctp", "../sctp", 1),
		"versioned override": strings.Replace(valid, "replace github.com/pion/sctp", "replace github.com/pion/sctp v1.12.0", 1),
		"remote fork":        strings.Replace(valid, "./third_party/pion/sctp", "example.com/sctp v1.12.0", 1),
		"duplicate override": valid + "replace github.com/pion/sctp v1.12.1 => ../sctp\n",
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
