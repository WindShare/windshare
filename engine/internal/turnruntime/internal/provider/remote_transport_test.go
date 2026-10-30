package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

func newRemoteTransportSource(t *testing.T, handler http.HandlerFunc) *Remote {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	client := remoteHTTPClient()
	// Only the network is substituted; client timeouts and redirect policy remain production-owned.
	client.Transport = server.Client().Transport
	source, err := NewRemote(RemoteConfig{
		RelayURL: "https://relay.example", AccessKey: "device-key", ServiceID: "cf-service", Client: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestRemoteTransportRetryCannotExtendDeadline(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var requests []remoteRequest
				canceled := false
				source := newRemoteTransportSource(t, func(w http.ResponseWriter, r *http.Request) {
					var request remoteRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					requests = append(requests, request)
					if len(requests) == 1 {
						// Spend part of the operation deadline before a retryable response.
						time.Sleep(HTTPTimeout / 2)
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, `{"error":"lease_pending"}`)
						return
					}
					if phase == "body" {
						w.WriteHeader(http.StatusCreated)
						_, _ = io.WriteString(w, `{"lease_id":`)
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
				started := time.Now()
				acquired, err := source.Issue(t.Context(), Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != HTTPTimeout {
					t.Fatalf("issue error=%v elapsed=%s, want original deadline %s", err, time.Since(started), HTTPTimeout)
				}
				synctest.Wait()
				if len(requests) != 2 || requests[0] != requests[1] || !canceled {
					t.Fatalf("retry requests=%+v canceled=%t", requests, canceled)
				}
				if acquired.Credentials.Kind() != 0 || acquired.Cleanup == nil || acquired.Cleanup.ID != requests[0].LeaseID {
					t.Fatalf("timed-out issuance lost cleanup ownership: %+v", acquired)
				}
			})
		})
	}
}

func TestRemoteTransportCancellationStopsBodyReadAndRetryWait(t *testing.T) {
	for _, phase := range []string{"body", "retry wait"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var requests []remoteRequest
				source := newRemoteTransportSource(t, func(w http.ResponseWriter, r *http.Request) {
					var request remoteRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					requests = append(requests, request)
					if phase == "retry wait" {
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, `{"error":"lease_pending"}`)
						return
					}
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, `{"lease_id":`)
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Error(err)
						return
					}
					<-r.Context().Done()
					panic(http.ErrAbortHandler)
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var acquired Acquisition
				var issueErr error
				done := make(chan struct{})
				go func() {
					acquired, issueErr = source.Issue(ctx, Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
					close(done)
				}()
				synctest.Sleep(remoteRetryDelay / 2)
				select {
				case <-done:
					t.Fatalf("issuance completed before cancellation: %v", issueErr)
				default:
				}
				if len(requests) != 1 {
					t.Fatalf("requests before cancellation = %d, want 1", len(requests))
				}
				canceledAt := time.Now()
				cancel()
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("cancellation did not interrupt the blocked operation")
				}
				if !errors.Is(issueErr, context.Canceled) || time.Since(canceledAt) != 0 {
					t.Fatalf("cancellation error=%v elapsed=%s", issueErr, time.Since(canceledAt))
				}
				if acquired.Credentials.Kind() != 0 || acquired.Cleanup == nil || acquired.Cleanup.ID != requests[0].LeaseID {
					t.Fatalf("canceled issuance lost cleanup ownership: %+v", acquired)
				}
				synctest.Sleep(remoteRetryDelay)
				if len(requests) != 1 {
					t.Fatal("canceled issuance sent another request")
				}
			})
		})
	}
}

func TestRemoteTransportReplaysTruncatedGrantWithSameLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests []remoteRequest
		var granted []byte
		started := time.Now()
		source := newRemoteTransportSource(t, func(w http.ResponseWriter, r *http.Request) {
			var request remoteRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			requests = append(requests, request)
			if granted == nil {
				var err error
				granted, err = json.Marshal(remoteGrant{
					ServiceID: request.ServiceID, LeaseID: request.LeaseID,
					SessionID: request.SessionID, PathID: request.PathID, URLs: []string{"turn:localhost:3478"},
					Username: "temporary-user", Password: "temporary-password", ExpiresAt: started.Add(ManagedLeaseLifetime).UnixMilli(),
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
			w.Header().Set("Date", started.UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusCreated)
			if len(requests) == 1 {
				_, _ = w.Write(granted[:len(granted)/2])
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
					return
				}
				// A broken chunked response exercises Transport's unexpected-EOF path.
				panic(http.ErrAbortHandler)
			}
			_, _ = w.Write(granted)
		})
		acquired, err := source.Issue(t.Context(), Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
		if err != nil || !acquired.Credentials.ValidAt(time.Now()) {
			t.Fatalf("replayed grant=%+v error=%v", acquired, err)
		}
		if len(requests) != 2 || requests[0] != requests[1] || time.Since(started) != remoteRetryDelay {
			t.Fatalf("replay requests=%+v elapsed=%s", requests, time.Since(started))
		}
		if acquired.Cleanup == nil || acquired.Cleanup.ID != requests[0].LeaseID ||
			acquired.Credentials.ExpiresAt().After(started.Add(ManagedLeaseLifetime)) {
			t.Fatal("replay changed cleanup identity or extended issuer authority")
		}
	})
}
