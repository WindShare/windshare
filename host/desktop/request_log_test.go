package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/relaysetup"
	"github.com/windshare/windshare/host/desktop/logstore"
)

func TestControlLogsExcludeCredentialsFromAcceptedAndRejectedRequests(t *testing.T) {
	const key = "relay-key-fixture"
	const relay = "https://user:password-fixture@relay.example:8443/private-path?token=query-fixture#fragment-fixture"
	directory := t.TempDir()
	log, err := logstore.Open(directory, "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	a, _, _ := fixture(t)
	a.deps.log = log.Record
	a.deps.setup = func(_ context.Context, setup SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		if setup.AccessKey != key || setup.RelayURL != relay {
			t.Error("setup workflow did not receive its original input")
		}
		return SetupResult{RelayURL: setup.RelayURL}, errors.New("setup rejected access key " + setup.AccessKey)
	}
	setup := SetupInput{ID: "relay-setup", RelayURL: relay, RelayOperation: relaysetup.AddProfile, AccessKey: key, FrontURL: "invalid frontend"}
	reply := a.execute(Request{ID: "invalid-setup", Command: "configure", Setup: &setup})
	if reply.Error == nil {
		t.Fatal("invalid frontend was accepted")
	}
	setup.FrontURL = ""
	reply = a.execute(Request{ID: "accepted-setup", Command: "configure", Setup: &setup})
	if reply.Error != nil {
		t.Fatalf("setup was not accepted: %v", reply.Error)
	}
	const password = "account-password-fixture"
	reply = a.execute(Request{ID: "account-login", Command: "account", OperationID: "account-operation",
		Account: &AccountInput{Action: "login", Username: "user", Password: password}})
	if reply.Error == nil {
		t.Fatal("account operation was accepted without an account service")
	}
	a.work.Wait()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(directory, "host-*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one host log file, got %v (%v)", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, secret := range []string{key, password, "password-fixture", "private-path", "query-fixture", "fragment-fixture"} {
		if strings.Contains(body, secret) {
			t.Fatalf("log contains credential-bearing input %q", secret)
		}
	}
	for _, fields := range [][]string{
		{`"event":"host.request_result"`, `"request_id":"invalid-setup"`, `"accepted":false`, `"error_code":"request_rejected"`},
		{`"event":"host.request_result"`, `"request_id":"accepted-setup"`, `"setup_id":"relay-setup"`, `"accepted":true`, `"relay_origin":"https://relay.example:8443"`},
		{`"event":"host.request_result"`, `"request_id":"account-login"`, `"operation_id":"account-operation"`, `"action":"login"`, `"accepted":false`},
		{`"event":"host.setup_completed"`, `"setup_id":"relay-setup"`, `"state":"failed"`, `"error_code":"setup_failed"`},
	} {
		found := false
		for line := range strings.SplitSeq(body, "\n") {
			matches := true
			for _, field := range fields {
				matches = matches && strings.Contains(line, field)
			}
			found = found || matches
		}
		if !found {
			t.Fatalf("no log event has fields %v:\n%s", fields, body)
		}
	}
}
