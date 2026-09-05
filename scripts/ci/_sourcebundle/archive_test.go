package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/module"
)

func TestSourceBundleUsesCommittedModesAndRejectsExtractionInjection(t *testing.T) {
	stage := t.TempDir()
	writeFile(t, filepath.Join(stage, "install.sh"), "#!/bin/sh\n")
	archive := filepath.Join(t.TempDir(), "source.zip")
	version := module.Version{Path: modulePath, Version: testModuleVersion}
	if err := createSourceBundle(archive, version, stage, []committedFile{{relativePath: "install.sh", executable: true}}); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	if reader.File[0].Mode().Perm() != 0o755 {
		t.Fatal("commit execute bit lost")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractSourceBundle(archive, t.TempDir(), version, []string{"different.sh"}); err == nil {
		t.Fatal("unexpected archive entry accepted")
	}
	output := filepath.Join(t.TempDir(), "malformed.zip")
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, name := range []string{sourcePrefix(version) + "install.sh", sourcePrefix(version) + "../escape"} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("content")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractSourceBundle(output, t.TempDir(), version, []string{"install.sh"}); err == nil {
		t.Fatal("traversal accepted")
	}
}
