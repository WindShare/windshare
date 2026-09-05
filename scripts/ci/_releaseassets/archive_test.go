package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackagedProgramsAndLicense(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"wind", "wsrelay", "LICENSE"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "release.zip")
	if err := archiveDirectory(archive, directory, "windshare-v0.1.0/", ""); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(reader.File) != 3 {
		t.Fatal(len(reader.File))
	}
	for _, file := range reader.File {
		if strings.HasSuffix(file.Name, "/LICENSE") {
			if file.Mode().Perm() != 0o644 {
				t.Fatal(file.Mode())
			}
		} else if file.Mode().Perm() != 0o755 {
			t.Fatal(file.Mode())
		}
	}
	if err := archiveDirectory(archive, directory, "prefix/", ""); err == nil {
		t.Fatal("overwrote existing archive")
	}
}

func TestAssetInputAndBuildEnvironment(t *testing.T) {
	for _, value := range []string{"", "../v0.1.0", "v0/1", "v1;bad"} {
		if safeIdentity(value) {
			t.Fatal(value)
		}
	}
	if !safeIdentity("v0.1.0") {
		t.Fatal("version rejected")
	}
	t.Setenv("GOFLAGS", "-overlay=outside.json")
	t.Setenv("GOOS", "alien")
	for _, item := range buildEnvironment() {
		if strings.Contains(item, "outside.json") || item == "GOOS=alien" {
			t.Fatal(item)
		}
	}
	if err := buildAssets("", "", "", "", ""); err == nil {
		t.Fatal("missing identity accepted")
	}
}
