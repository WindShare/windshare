package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
	"github.com/windshare/windshare/internal/diaglog"
)

func TestPreferenceRequestLogsUseOnlyDiagnosticFields(t *testing.T) {
	const relay = "wss://user:password@relay.example:8443/private?route=relay-token#relay-fragment"
	const frontend = "https://user:password@frontend.example/private?token=frontend-token#frontend-fragment"
	tests := []struct {
		name           string
		edit           relayconfig.PreferenceEdit
		action         string
		relayOrigin    string
		frontendOrigin string
	}{
		{"enable", relayconfig.PreferenceEdit{Action: "enable", Value: relay}, "enable", "wss://relay.example:8443", ""},
		{"disable", relayconfig.PreferenceEdit{Action: "disable", Value: relay}, "disable", "wss://relay.example:8443", ""},
		{"remove", relayconfig.PreferenceEdit{Action: "remove", Value: relay}, "remove", "wss://relay.example:8443", ""},
		{"frontend", relayconfig.PreferenceEdit{Action: "frontend", Value: frontend}, "frontend", "", "https://frontend.example"},
		{"unicode frontend", relayconfig.PreferenceEdit{Action: "frontend", Value: "https://user:password@文件.EXAMPLE:443/private?token=t#frag"}, "frontend", "", "https://xn--5nqy36c.example"},
		{"invalid address", relayconfig.PreferenceEdit{Action: "frontend", Value: "private text"}, "frontend", "", ""},
		{"restore default", relayconfig.PreferenceEdit{Action: "frontend"}, "frontend", "", ""},
		{"mode", relayconfig.PreferenceEdit{Action: "mode", Value: "selected"}, "mode", "", ""},
		{"community nodes", relayconfig.PreferenceEdit{Action: "community_nodes", Value: "off"}, "community_nodes", "", ""},
		{"non-canonical community action", relayconfig.PreferenceEdit{Action: "community", Value: "on"}, "", "", ""},
		{"unknown action", relayconfig.PreferenceEdit{Action: "private text", Value: relay}, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logged requestResultLog
			a := Application{deps: dependencies{log: func(_ string, fields any) {
				logged = fields.(requestResultLog)
			}}}
			a.logRequestResult(Request{
				ID: "request-id", Command: "configure", OperationID: "share-id",
				Setup: &SetupInput{ID: "setup-id", Preference: &tt.edit},
			}, nil)
			want := requestResultLog{
				RequestID: "request-id", Command: "configure", OperationID: "share-id", SetupID: "setup-id",
				Action: tt.action, RelayOrigin: tt.relayOrigin, FrontendOrigin: tt.frontendOrigin, Accepted: true,
			}
			if logged != want {
				t.Fatalf("log fields = %+v, want %+v", logged, want)
			}
		})
	}
}

func TestControlLogsExcludeCredentialsFromAcceptedAndRejectedRequests(t *testing.T) {
	const key = "relay-key-fixture"
	const relay = "https://user:password-fixture@relay.example:8443/private-path?token=query-fixture#fragment-fixture"
	directory := t.TempDir()
	log, err := diaglog.OpenRotating(directory, "session")
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
	a.logShareState(Share{
		OperationID: "share-fixture", URL: "https://example.test/s/sample#share-key-fixture",
		BackupURL: "https://backup.example/s/sample#backup-key-fixture",
		Selection: &Selection{Root: &SelectedRoot{Name: "private-file-fixture"}}, State: Sharing,
		Error: &Failure{
			Code:       "startup_failed",
			EngineCode: "selection_collision",
			Message:    "private-error-message-fixture",
			SourcePath: `C:\Users\secret\private-source-fixture.docx`,
			SelectionCollision: &engine.SelectionCollision{
				FirstPath:  `C:\Users\secret\collision-first-fixture.txt`,
				SecondPath: `D:\Users\secret\collision-second-fixture.txt`,
			},
		},
		Omissions: &SourceOmissions{
			Count: 3,
			Samples: []SourceOmission{
				{Name: "private-omitted-fixture.docx", Reason: "cloud_placeholder", Detail: "private-omission-detail-fixture"},
			},
		},
	})
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
	for _, secret := range []string{
		key, password, "password-fixture", "private-path", "query-fixture", "fragment-fixture",
		"share-key-fixture", "backup-key-fixture", "private-file-fixture",
		"private-error-message-fixture", "private-source-fixture",
		"collision-first-fixture", "collision-second-fixture",
		"private-omitted-fixture", "private-omission-detail-fixture",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("log contains credential-bearing input %q", secret)
		}
	}
	for _, fields := range [][]string{
		{`"event":"host.request_result"`, `"request_id":"invalid-setup"`, `"accepted":false`, `"error_code":"request_rejected"`},
		{`"event":"host.request_result"`, `"request_id":"accepted-setup"`, `"setup_id":"relay-setup"`, `"accepted":true`, `"relay_origin":"https://relay.example:8443"`},
		{`"event":"host.request_result"`, `"request_id":"account-login"`, `"operation_id":"account-operation"`, `"action":"login"`, `"accepted":false`},
		{`"event":"host.setup_completed"`, `"setup_id":"relay-setup"`, `"state":"failed"`, `"error_code":"setup_failed"`},
		{`"event":"host.share_state"`, `"operation_id":"share-fixture"`, `"state":"sharing"`, `"engine_error_code":"selection_collision"`, `"omitted_count":3`},
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
