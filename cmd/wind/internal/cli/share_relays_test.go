package cli

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"reflect"
	"testing"
)

func TestShareLoadsSavedCandidatesAndOnlyOverridesSelection(t *testing.T) {
	saved := []senderrelay.Candidate{
		{PublicURL: "https://one.example", RegistrationToken: "one-private-token"},
		{PublicURL: "https://two.example", RegistrationToken: "two-private-token"},
	}
	app := testApp("")
	app.loadRelayCandidates = func() ([]senderrelay.Candidate, error) { return saved, nil }
	for range 2 {
		request, outcome := app.parseShareRequest([]string{"root"})
		if outcome != requestParseReady || !reflect.DeepEqual(request.relays, saved) {
			t.Fatal("ordinary share lost saved credentials")
		}
	}
	request, outcome := app.parseShareRequest([]string{"root", "--relay", "wss://TWO.example:443/"})
	if outcome != requestParseReady || len(request.relays) != 1 || request.relays[0].RegistrationToken != "two-private-token" {
		t.Fatal("explicit selection lost bound credential")
	}
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	failureApp := &App{Stdout: stdout, Stderr: stderr,
		loadRelayCandidates: func() ([]senderrelay.Candidate, error) { return nil, errors.New("private-error-token") },
	}
	t.Cleanup(failureApp.closeTerminalOutput)
	if _, outcome := failureApp.parseShareRequest([]string{"root"}); outcome != requestParseUsageFailure {
		t.Fatal("invalid saved configuration was ignored")
	}
	if stderr.Len() == 0 || bytes.Contains(stderr.Bytes(), []byte("private-error-token")) || stdout.Len() != 0 {
		t.Fatal("configuration failure exposed private input")
	}
}

func TestShareRelayConfigurationIsPluralBoundedAndDeduplicated(t *testing.T) {
	app := testApp("")
	request, outcome := app.parseShareRequest([]string{"root", "--relay", "https://a.example", "--relay", "https://b.example", "--relay", "https://a.example"})
	if outcome != requestParseReady || !reflect.DeepEqual(request.relays, []senderrelay.Candidate{{PublicURL: "https://a.example"}, {PublicURL: "https://b.example"}}) {
		t.Fatalf("relays=%v outcome=%v", request.relays, outcome)
	}
	request, outcome = app.parseShareRequest([]string{"root"})
	if outcome != requestParseReady || !reflect.DeepEqual(request.relays, []senderrelay.Candidate{{PublicURL: DefaultRelayURL}}) {
		t.Fatal("default relay missing")
	}
	args := []string{"root"}
	for index := range relayset.MaximumEndpoints + 1 {
		args = append(args, "--relay", fmt.Sprintf("https://relay-%d.example", index))
	}
	if _, outcome = app.parseShareRequest(args); outcome != requestParseUsageFailure {
		t.Fatal("unbounded relay configuration accepted")
	}
}
