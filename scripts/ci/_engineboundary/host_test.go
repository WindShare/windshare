package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopHostsRejectNetworkingAndTestSupportButAssembleFilesystem(t *testing.T) {
	root := t.TempDir()
	cases := map[string]string{
		"host/desktop/transport.go":          "/transport/relayv2",
		"host/desktop/connectivity_plan9.go": "/connectivity/relayset",
		"host/desktop/protocol.go":           "/relay/protocol/v2",
		"host/desktop/fixture.go":            "/internal/testrelaynative",
		"cmd/windshare-host/fixture.go":      "/internal/testsupport",
		"host/desktop/source.go":             "/adapters/filesystem",
		"cmd/windshare-host/source.go":       "/osfs",
		"host/desktop/fixture_test.go":       "/internal/testrelaynative",
		"cmd/testprocessowner/fixture.go":    "/internal/testprocess",
	}
	for path, dependency := range cases {
		target := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		body := "package host\nimport _ \"" + module + dependency + "\"\n"
		if err := os.WriteFile(target, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	findings, err := sourceFindings(root)
	if err != nil || len(findings) != 5 {
		t.Fatalf("findings=%v err=%v", findings, err)
	}
}
