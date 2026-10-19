package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/relayconfig"
	"github.com/windshare/windshare/connectivity/senderrelay"
)

func TestRelayImportFeedsOrdinaryShareWithoutStartingAnEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relays.json")
	t.Setenv(relayconfig.PathEnvironment, path)
	var stdout, stderr bytes.Buffer
	app := &App{
		Stdin:  strings.NewReader(`{"relays":[{"url":"https://private.example","registration_token":"private-import-canary"}]}`),
		Stdout: &stdout, Stderr: &stderr,
		saveRelayCandidates: relayconfig.Save, loadRelayCandidates: relayconfig.Load,
	}
	t.Cleanup(app.closeTerminalOutput)
	if code := app.Run(context.Background(), []string{"relay", "import", "-"}); code != ExitOK {
		t.Fatal("import failed:", code, stderr.String())
	}
	if app.application != nil || app.revisionCapacityTrace != nil {
		t.Fatal("local configuration import constructed a transfer engine")
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "private-import-canary") {
		t.Fatal("import leaked configuration or wrote to the capability stream")
	}
	expected := []senderrelay.Candidate{{PublicURL: "https://private.example", RegistrationToken: "private-import-canary"}}
	request, outcome := app.parseShareRequest([]string{"root"})
	if outcome != requestParseReady || !reflect.DeepEqual(request.relays, expected) {
		t.Fatal("ordinary share did not use the imported complete candidate set")
	}
	// Reading and replacing the saved file itself must work on Windows too.
	if code := app.Run(context.Background(), []string{"relay", "import", path}); code != ExitOK {
		t.Fatal("re-import of the saved file failed:", code)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	app.Stdin = strings.NewReader(`{"relays":[]}`)
	if code := app.Run(context.Background(), []string{"relay", "import", "-"}); code != ExitUsage {
		t.Fatal("invalid import did not return a usage error")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid import changed the saved selection:", err)
	}
}

func TestRelayImportMapsStorageFailure(t *testing.T) {
	app := testApp(`{"relays":[{"url":"https://relay.example"}]}`)
	app.saveRelayCandidates = func([]senderrelay.Candidate) error { return os.ErrPermission }
	t.Cleanup(app.closeTerminalOutput)
	if code := app.Run(context.Background(), []string{"relay", "import", "-"}); code != ExitFailure {
		t.Fatal("storage error did not fail the command:", code)
	}
}
