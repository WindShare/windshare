package main

import (
	"archive/zip"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
)

func TestSourceBundleUsesCommittedModesAndRejectsExtractionInjection(t *testing.T) {
	repositoryRoot, _ := newModuleRepository(t)
	writeFile(t, filepath.Join(repositoryRoot, "install.sh"), "#!/bin/sh\n")
	runGit(t, repositoryRoot, "add", "--", "install.sh")
	runGit(t, repositoryRoot, "update-index", "--chmod=+x", "install.sh")
	runGit(t, repositoryRoot, "commit", "--quiet", "-m", "executable install script")
	commitSHA := strings.TrimSpace(gitOutputForTest(t, repositoryRoot, "rev-parse", "HEAD"))

	files, err := committedModuleFiles(repositoryRoot, commitSHA)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "source.zip")
	version := module.Version{Path: modulePath, Version: testModuleVersion}
	bundle, err := buildSourceBundle(repositoryRoot, archive, version, files)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	foundExecutable := false
	for _, entry := range reader.File {
		if entry.Name == sourcePrefix(version)+"install.sh" {
			foundExecutable = true
			if entry.Mode().Perm() != 0o755 {
				t.Fatalf("commit execute bit lost: got %o", entry.Mode().Perm())
			}
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !foundExecutable {
		t.Fatal("install.sh missing from source bundle")
	}

	if err := extractSourceBundle(archive, t.TempDir(), version, []string{"different.sh"}, nil); err == nil {
		t.Fatal("unexpected archive entry accepted")
	}

	wrongDigests := make(map[string][sha256.Size]byte, len(bundle.fileDigests))
	for key, digest := range bundle.fileDigests {
		wrongDigests[key] = digest
	}
	wrongDigests["install.sh"] = sha256.Sum256([]byte("tampered"))
	if err := extractSourceBundle(archive, t.TempDir(), version, committedFilePaths(files), wrongDigests); err == nil || !strings.Contains(err.Error(), "extracted source differs") {
		t.Fatalf("digest mismatch error = %v, want extracted source differs", err)
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
	if err := extractSourceBundle(output, t.TempDir(), version, []string{"install.sh"}, nil); err == nil {
		t.Fatal("traversal accepted")
	}
}
