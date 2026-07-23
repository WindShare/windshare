//go:build !race

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadManifestPinsRetiredConnectivityTombstone(t *testing.T) {
	t.Parallel()
	document := validManifestDocument()
	manifest, err := loadManifest(writeManifest(t, document))
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.packages["transport/webrtc"] || len(manifest.packages) != 1 {
		t.Fatalf("active packages = %#v, want exact fixture package", manifest.packages)
	}
	if !manifest.reservedExecutableNames[retiredConnectivityProgram] ||
		len(manifest.reservedExecutableNames) != 1 {
		t.Fatalf("reserved executable names = %#v, want exact connectivity tombstone", manifest.reservedExecutableNames)
	}
}

func TestLoadManifestRejectsBroadenedOrReintroducedTombstone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*manifestDocument)
	}{
		{
			name: "wildcard tombstone",
			mutate: func(document *manifestDocument) {
				document.RetiredProgramTombstone.RelativeProgram = "*.test.exe"
			},
		},
		{
			name: "unlisted tombstone",
			mutate: func(document *manifestDocument) {
				document.RetiredProgramTombstone.RelativeProgram = "other.test.exe"
			},
		},
		{
			name: "live action",
			mutate: func(document *manifestDocument) {
				document.RetiredProgramTombstone.Action = "Allow"
			},
		},
		{
			name: "reintroduced active package",
			mutate: func(document *manifestDocument) {
				document.Packages = append(document.Packages, packageRecord{
					Name: "connectivity",
					Path: "./connectivity",
				})
			},
		},
		{
			name: "duplicate rule identity",
			mutate: func(document *manifestDocument) {
				document.RetiredProgramTombstone.UDPGuid = document.RetiredProgramTombstone.TCPGuid
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validManifestDocument()
			test.mutate(&document)
			if _, err := loadManifest(writeManifest(t, document)); err == nil {
				t.Fatal("loadManifest succeeded, want strict tombstone rejection")
			}
		})
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
		`"SchemaVersion":2`,
		`"SchemaVersion":2,"WildcardRetirements":[]`,
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
		RetiredProgramTombstone: retiredProgramTombstone{
			RelativeProgram: retiredConnectivityProgram,
			DisplayName:     retiredConnectivityDisplay,
			Action:          "Block",
			TCPGuid:         "E9A64ACF-24D6-4B94-91CE-D8E468113113",
			UDPGuid:         "6A523662-D935-4D63-BE14-EEC446E3B720",
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
