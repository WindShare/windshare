//go:build !race

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadManifestPinsExactPackageSet(t *testing.T) {
	t.Parallel()
	document := validManifestDocument()
	manifest, err := loadManifest(writeManifest(t, document))
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.packages["transport/webrtc"] || len(manifest.packages) != 1 {
		t.Fatalf("active packages = %#v, want exact fixture package", manifest.packages)
	}
}

func TestLoadManifestRejectsUnknownOrTrailingJSON(t *testing.T) {
	t.Parallel()
	valid := validManifestDocument()
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(
		string(raw),
		`"SchemaVersion":3`,
		`"SchemaVersion":3,"UnexpectedPolicy":[]`,
		1,
	)
	for name, content := range map[string]string{
		"unknown":  unknown,
		"trailing": string(raw) + ` {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadManifest(path); err == nil {
				t.Fatal("loadManifest succeeded, want strict JSON rejection")
			}
		})
	}
}

func validManifestDocument() manifestDocument {
	return manifestDocument{
		SchemaVersion: networkManifestSchemaVersion,
		Packages: []packageRecord{
			{Name: "webrtc", Path: "./transport/webrtc"},
		},
	}
}

func writeManifest(t *testing.T, document manifestDocument) string {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
