package cli

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestConfigImportFeedsOrdinaryShareWithoutStartingAnEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(relayconfig.PathEnvironment, path)
	var stdout, stderr bytes.Buffer
	app := &App{
		Stdin:  strings.NewReader(`{"active_relays":["https://private.example"],"profiles":[{"url":"https://private.example","access_key":"private-import-canary"}]}`),
		Stdout: &stdout, Stderr: &stderr,
		saveShareConfig: relayconfig.Save, loadShareConfig: relayconfig.Load,
	}
	t.Cleanup(app.closeTerminalOutput)
	if code := app.Run(context.Background(), []string{"config", "import", "-", "--front-url", "https://private.example"}); code != ExitOK {
		t.Fatal("import failed:", code, stderr.String())
	}
	if app.application != nil || app.revisionCapacityTrace != nil {
		t.Fatal("local configuration import constructed a transfer engine")
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "private-import-canary") {
		t.Fatal("import leaked configuration or wrote to the capability stream")
	}
	expected := []relayprofile.Candidate{{PublicURL: "https://private.example", AccessKey: "private-import-canary"}}
	request, outcome := app.parseShareRequest([]string{"root"})
	if outcome != requestParseReady || !reflect.DeepEqual(request.relays, expected) || request.link.frontURL != "https://private.example" {
		t.Fatal("ordinary share did not use the imported complete candidate set")
	}
	capability := testShareCapability(t)
	capability.Relays = []string{request.relays[0].PublicURL}
	payload, err := buildShareCapabilityPayload(capability, request.link)
	if err != nil {
		t.Fatal(err)
	}
	linkText := strings.TrimSpace(strings.TrimPrefix(string(payload), "Link: "))
	page, err := url.Parse(linkText)
	if err != nil || page.Scheme != "https" || page.Host != "private.example" || page.RawQuery != "" {
		t.Fatal("imported same-origin deployment did not produce a clean public link")
	}
	parsed, err := link.Parse(linkText)
	if err != nil || !reflect.DeepEqual(parsed, capability) {
		t.Fatal("omitting the relay hint changed the receiver's capability")
	}
	// Reading and replacing the saved file itself must work on Windows too.
	if code := app.Run(context.Background(), []string{"config", "import", path}); code != ExitOK {
		t.Fatal("re-import of the saved file failed:", code)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	app.Stdin = strings.NewReader(`{"active_relays":["https://private.example"],"profiles":[]}`)
	if code := app.Run(context.Background(), []string{"config", "import", "-"}); code != ExitUsage {
		t.Fatal("invalid import did not return a usage error")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid import changed the saved selection:", err)
	}
}

func TestConfigImportMapsStorageFailure(t *testing.T) {
	app := testApp(`{"active_relays":["https://relay.example"],"profiles":[{"url":"https://relay.example"}]}`)
	app.saveShareConfig = func(relayconfig.Config) error { return os.ErrPermission }
	t.Cleanup(app.closeTerminalOutput)
	if code := app.Run(context.Background(), []string{"config", "import", "-"}); code != ExitFailure {
		t.Fatal("storage error did not fail the command:", code)
	}
}
