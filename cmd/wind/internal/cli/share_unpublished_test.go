package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/relayconfig"
)

func TestShareCancellationBeforePublicationSettlesWithoutRedialing(t *testing.T) {
	for _, rejected := range []bool{true, false} {
		name := "hung upgrade"
		if rejected {
			name = "rejected upgrade"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), cliShareCancellationTimeout)
			defer cancel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if rejected {
					http.NotFound(w, r)
					w.(http.Flusher).Flush()
				}
				cancel()
				if !rejected {
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(file, []byte("unpublished share"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			trace := newRecordingUserTrace()
			app := &App{
				Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""),
				engineConfig: engine.Config{Share: engine.ShareDependencies{
					PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
				}},
				openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
					return trace, nil
				},
			}
			code := app.Run(ctx, []string{"share", file, "--relay", server.URL, "--front-url", "https://front.example", "--trace", filepath.Join(t.TempDir(), "share.ndjson")})
			if code != ExitOK || stdout.Len() != 0 || requests.Load() != 1 {
				t.Fatalf("exit=%d stdout=%q requests=%d stderr=%q", code, stdout.String(), requests.Load(), stderr.String())
			}
			trace.mu.Lock()
			defer trace.mu.Unlock()
			terminal := 0
			for _, event := range trace.events {
				switch event := event.(type) {
				case clievent.SharingStopped:
					terminal++
					if event.Result().Publication() != clievent.ShareUnpublished || !event.Result().StoppedCleanly() {
						t.Fatalf("unpublished stop result = %+v", event.Result())
					}
				case appobs.Ready, clievent.CommandFailed:
					t.Fatalf("unpublished cancellation emitted %T", event)
				}
			}
			if terminal != 1 {
				t.Fatalf("terminal results = %d, want one unpublished stop", terminal)
			}
		})
	}
}

type staticAccountClient func(*http.Request) (*http.Response, error)

func (call staticAccountClient) Do(request *http.Request) (*http.Response, error) {
	return call(request)
}

func TestShareFailsFastWhenOfficialAccountActionIsRequired(t *testing.T) {
	var directoryFixture struct {
		Now     time.Time
		Roots   []struct{ ID, Key string }
		Entries []struct{ Envelope json.RawMessage }
	}
	dirData, err := os.ReadFile("../../../../testdata/network/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(dirData, &directoryFixture); err != nil {
		t.Fatal(err)
	}
	dirKey, err := base64.RawURLEncoding.DecodeString(directoryFixture.Roots[0].Key)
	if err != nil {
		t.Fatal(err)
	}

	var authorityFixture struct {
		Now  int64
		Root string
	}
	authData, err := os.ReadFile("../../../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(authData, &authorityFixture); err != nil {
		t.Fatal(err)
	}
	rootKey, err := serviceauthority.PublicKey(authorityFixture.Root)
	if err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name      string
		challenge account.Challenge
		wantHint  string
	}{
		{
			name:      "proof of work",
			challenge: account.Challenge{ID: "pow-1", Mode: "proof_of_work", Bits: 4, ExpiresAt: uint64(directoryFixture.Now.Unix()) + 300},
			wantHint:  "wind account register",
		},
		{
			name:      "email verification",
			challenge: account.Challenge{Mode: "email", ExpiresAt: uint64(directoryFixture.Now.Unix()) + 300},
			wantHint:  "wind account email <address>",
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), cliShareCancellationTimeout)
			defer cancel()

			file := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(file, []byte("needs account action"), 0o600); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"kind": "verification", "challenge": scenario.challenge})
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			trace := newRecordingUserTrace()
			app := &App{
				Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""),
				clock:           newSystemCommandClock(func() time.Time { return directoryFixture.Now }),
				loadShareConfig: func() (relayconfig.Config, error) { return relayconfig.Config{}, nil },
				engineConfig: engine.Config{
					Network: &networkconfig.Config{
						LinkHost: "https://front.example",
						Directory: networkdirectory.Config{
							Network:  "test",
							Roots:    []nodedirectory.Root{{ID: directoryFixture.Roots[0].ID, Key: dirKey}},
							Embedded: directoryFixture.Entries[0].Envelope,
							Fetch: func(context.Context, string) ([]byte, error) {
								return directoryFixture.Entries[0].Envelope, nil
							},
						},
						Authority: &account.Config{
							URL:   "https://api.example",
							Root:  rootKey,
							Store: &account.MemoryStore{},
							Now:   func() time.Time { return directoryFixture.Now },
							Client: staticAccountClient(func(request *http.Request) (*http.Response, error) {
								if request.URL.Path == "/v1/accounts" {
									return &http.Response{
										StatusCode: http.StatusPreconditionRequired,
										Body:       io.NopCloser(bytes.NewReader(body)),
									}, nil
								}
								return &http.Response{
									StatusCode: http.StatusNotFound,
									Body:       io.NopCloser(strings.NewReader("")),
								}, nil
							}),
						},
					},
					Share: engine.ShareDependencies{
						PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
					},
				},
				openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
					return trace, nil
				},
			}
			code := app.Run(ctx, []string{"share", file, "--trace", filepath.Join(t.TempDir(), "share.ndjson")})
			if code != ExitNetwork || stdout.Len() != 0 {
				t.Fatalf("exit=%d want=%d stdout=%q stderr=%q", code, ExitNetwork, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), scenario.wantHint) {
				t.Fatalf("stderr=%q missing actionable hint %q", stderr.String(), scenario.wantHint)
			}
			trace.mu.Lock()
			defer trace.mu.Unlock()
			var failures int
			for _, event := range trace.events {
				if failed, ok := event.(clievent.CommandFailed); ok {
					failures++
					if failed.Failure().Code() != appobs.FailureRelayAccessDenied {
						t.Fatalf("failure code = %v, want relay access denied", failed.Failure().Code())
					}
				}
			}
			if failures != 1 {
				t.Fatalf("command failures = %d, want 1", failures)
			}
		})
	}
}

func TestShareFailsFastWhenLocalAccountStoreIsInvalid(t *testing.T) {
	var directoryFixture struct {
		Now     time.Time
		Roots   []struct{ ID, Key string }
		Entries []struct{ Envelope json.RawMessage }
	}
	dirData, err := os.ReadFile("../../../../testdata/network/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(dirData, &directoryFixture); err != nil {
		t.Fatal(err)
	}
	dirKey, err := base64.RawURLEncoding.DecodeString(directoryFixture.Roots[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	var authorityFixture struct {
		Root string
	}
	authData, err := os.ReadFile("../../../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(authData, &authorityFixture); err != nil {
		t.Fatal(err)
	}
	rootKey, err := serviceauthority.PublicKey(authorityFixture.Root)
	if err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "invalid json",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non-regular file",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), cliShareCancellationTimeout)
			defer cancel()

			dir := t.TempDir()
			file := filepath.Join(dir, "file.txt")
			if err := os.WriteFile(file, []byte("needs local repair"), 0o600); err != nil {
				t.Fatal(err)
			}
			accountPath := filepath.Join(dir, "config.json.account.json")
			scenario.setup(t, accountPath)

			var stdout, stderr bytes.Buffer
			trace := newRecordingUserTrace()
			app := &App{
				Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""),
				clock:           newSystemCommandClock(func() time.Time { return directoryFixture.Now }),
				loadShareConfig: func() (relayconfig.Config, error) { return relayconfig.Config{}, nil },
				engineConfig: engine.Config{
					Network: &networkconfig.Config{
						LinkHost: "https://front.example",
						Directory: networkdirectory.Config{
							Network:  "test",
							Roots:    []nodedirectory.Root{{ID: directoryFixture.Roots[0].ID, Key: dirKey}},
							Embedded: directoryFixture.Entries[0].Envelope,
							Fetch: func(context.Context, string) ([]byte, error) {
								return directoryFixture.Entries[0].Envelope, nil
							},
						},
						Authority: &account.Config{
							URL:   "https://api.example",
							Root:  rootKey,
							Store: account.FileStore{Path: accountPath},
							Now:   func() time.Time { return directoryFixture.Now },
							Client: staticAccountClient(func(request *http.Request) (*http.Response, error) {
								t.Errorf("corrupt local account store contacted remote authority %s", request.URL.Path)
								return nil, context.Canceled
							}),
						},
					},
					Share: engine.ShareDependencies{
						PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
					},
				},
				openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
					return trace, nil
				},
			}
			code := app.Run(ctx, []string{"share", file, "--trace", filepath.Join(dir, "share.ndjson")})
			if code != ExitFailure || stdout.Len() != 0 {
				t.Fatalf("exit=%d want=%d stdout=%q stderr=%q", code, ExitFailure, stdout.String(), stderr.String())
			}
			for _, want := range []string{"local account file", "--relay"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr=%q missing %q", stderr.String(), want)
				}
			}
			trace.mu.Lock()
			defer trace.mu.Unlock()
			var failures int
			for _, event := range trace.events {
				if failed, ok := event.(clievent.CommandFailed); ok {
					failures++
					if failed.Failure().Code() != appobs.FailureAccountStorageError {
						t.Fatalf("failure code = %v, want account storage error", failed.Failure().Code())
					}
				}
			}
			if failures != 1 {
				t.Fatalf("command failures = %d, want 1", failures)
			}
		})
	}
}
