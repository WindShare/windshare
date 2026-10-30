package relaysetup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func newSetupTransportService(t *testing.T, store Store, handler http.HandlerFunc) *Service {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	service := New(Dependencies{Store: store})
	// Preserve the default HTTP policy and exercise the production clock and retry wait.
	service.dependencies.HTTP.(*http.Client).Transport = server.Client().Transport
	return service
}

func TestSetupTransportTimeoutPreservesConfiguration(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				initial := prior()
				store := &memoryStore{saved: initial}
				requests, canceled := 0, false
				service := newSetupTransportService(t, store, func(w http.ResponseWriter, r *http.Request) {
					requests++
					if phase == "body" {
						w.WriteHeader(http.StatusOK)
						_, _ = io.WriteString(w, `{"version":`)
						if err := http.NewResponseController(w).Flush(); err != nil {
							t.Error(err)
							return
						}
					}
					<-r.Context().Done()
					canceled = true
					// Do not turn a stalled response into a successful EOF on cancellation.
					panic(http.ErrAbortHandler)
				})
				timeout := service.dependencies.HTTP.(*http.Client).Timeout
				started := time.Now()
				_, err := service.Run(t.Context(), Request{URL: "https://relay.example", Operation: AddProfile}, nil)
				var failure *apiFailure
				if !errors.As(err, &failure) || !failure.temporary() || time.Since(started) != timeout {
					t.Fatalf("discovery error=%v elapsed=%s, want timeout after %s", err, time.Since(started), timeout)
				}
				synctest.Wait()
				if requests != 1 || !canceled {
					t.Fatalf("discovery requests=%d canceled=%t", requests, canceled)
				}
				if store.saved.SettingsRevision() != initial.SettingsRevision() || store.saved.Pending != nil {
					t.Fatal("failed discovery changed saved configuration")
				}
			})
		})
	}
}

func TestSetupTransportCancellationRetainsResumableEnrollment(t *testing.T) {
	for _, phase := range []string{"body", "retry wait"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const relayURL = "https://relay.example"
				const pollInterval = 5 * time.Second
				initial := prior()
				store := &memoryStore{saved: initial}
				enrollments, confirmations := 0, 0
				var claims []map[string]string
				service := newSetupTransportService(t, store, func(w http.ResponseWriter, r *http.Request) {
					var reply any
					switch r.URL.Path {
					case "/.well-known/windshare":
						reply = discovery{Version: 1, Access: "restricted", RelayURL: relayURL, ConsoleURL: relayURL}
					case "/api/enrollments":
						enrollments++
						reply = enrollmentResponse{
							ID: "enrollment", UserCode: "ABCD-EFGH", VerificationURL: relayURL + "/?code=ABCD-EFGH",
							ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), IntervalMS: pollInterval.Milliseconds(),
						}
					case "/api/enrollments/claim":
						var claim map[string]string
						if err := json.NewDecoder(r.Body).Decode(&claim); err != nil {
							t.Error(err)
							return
						}
						claims = append(claims, claim)
						if len(claims) == 1 {
							if phase == "retry wait" {
								w.WriteHeader(http.StatusAccepted)
								reply = apiFailure{ErrorCode: "authorization_pending", RetryMS: pollInterval.Milliseconds()}
								break
							}
							w.WriteHeader(http.StatusOK)
							_, _ = io.WriteString(w, `{"access_key":"partial`)
							if err := http.NewResponseController(w).Flush(); err != nil {
								t.Error(err)
								return
							}
							<-r.Context().Done()
							panic(http.ErrAbortHandler)
						}
						reply = credentialResponse{ID: "credential", AccessKey: "device-key"}
					case "/api/enrollments/installed":
						confirmations++
						reply = map[string]bool{"ok": true}
					default:
						t.Error("unexpected setup endpoint", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					if err := json.NewEncoder(w).Encode(reply); err != nil {
						t.Error(err)
					}
				})
				request := Request{URL: relayURL, DeviceName: "Laptop", Operation: PrivateOnboarding}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var runErr error
				done := make(chan struct{})
				go func() {
					_, runErr = service.Run(ctx, request, nil)
					close(done)
				}()
				synctest.Sleep(pollInterval / 2)
				select {
				case <-done:
					t.Fatalf("setup completed before cancellation: %v", runErr)
				default:
				}
				if len(claims) != 1 || store.saved.Pending == nil || store.saved.Pending.enrollment().ID != "enrollment" {
					t.Fatalf("setup did not reach the pending claim: claims=%v pending=%+v", claims, store.saved.Pending)
				}
				operationID := store.saved.Pending.OperationID
				canceledAt := time.Now()
				cancel()
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("cancellation did not interrupt the blocked setup")
				}
				if !errors.Is(runErr, context.Canceled) || time.Since(canceledAt) != 0 {
					t.Fatalf("setup cancellation error=%v elapsed=%s", runErr, time.Since(canceledAt))
				}
				if store.saved.SettingsRevision() != initial.SettingsRevision() || store.saved.Pending == nil ||
					store.saved.Pending.OperationID != operationID || store.saved.Pending.credential().AccessKey != "" {
					t.Fatal("cancellation changed configuration or lost the resumable transaction")
				}
				synctest.Sleep(pollInterval)
				if len(claims) != 1 {
					t.Fatal("canceled setup kept polling")
				}

				result, err := service.Run(t.Context(), request, nil)
				if err != nil || result.CredentialID != "credential" {
					t.Fatalf("resumed setup=%+v error=%v", result, err)
				}
				if enrollments != 1 || len(claims) != 2 || !maps.Equal(claims[0], claims[1]) || confirmations != 1 {
					t.Fatalf("resume changed enrollment identity: enrollments=%d claims=%v confirmations=%d", enrollments, claims, confirmations)
				}
				if store.saved.Pending != nil || len(store.saved.Profiles) != len(initial.Profiles)+1 ||
					store.saved.FrontURL != initial.FrontURL || !slices.Equal(store.saved.ActiveRelays, []string{relayURL}) {
					t.Fatal("resumed installation did not commit the private relay selection")
				}
			})
		})
	}
}
