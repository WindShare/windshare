package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

type lostResponseBody struct{}

func (lostResponseBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (lostResponseBody) Close() error             { return nil }

func TestRemoteReplaysLostRepliesWithOneLeaseIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests []remoteRequest
		var granted []byte
		source, _ := NewRemote(RemoteConfig{RelayURL: "https://relay.example", AccessKey: "key", ServiceID: "cf-service",
			Client: clientFunc(func(request *http.Request) (*http.Response, error) {
				var body remoteRequest
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, body)
				if granted == nil {
					granted, _ = json.Marshal(remoteGrant{ServiceID: body.ServiceID, LeaseID: body.LeaseID,
						SessionID: body.SessionID, PathID: body.PathID, URLs: []string{"turn:localhost:3478"},
						Username: "u", Password: "p", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
				}
				if len(requests) <= 2 {
					return &http.Response{StatusCode: http.StatusCreated, Body: lostResponseBody{}}, nil
				}
				return response(http.StatusOK, string(granted)), nil
			})})
		acquired, err := source.Issue(t.Context(), Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
		if err != nil || !acquired.Credentials.ValidAt(time.Now()) {
			t.Fatal(acquired, err)
		}
		if len(requests) != 3 || requests[0] != requests[1] || requests[1] != requests[2] {
			t.Fatalf("replay changed operation identity: %+v", requests)
		}
		if acquired.Cleanup == nil || acquired.Cleanup.ID != requests[0].LeaseID {
			t.Fatal("lost cleanup ownership")
		}
	})
}

func TestRemoteRetainsCleanupAfterCancellationOrInvalidGrant(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "invalid grant"}[invalid], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var issuance remoteRequest
			var revoked bool
			source, _ := NewRemote(RemoteConfig{RelayURL: "https://relay.example", AccessKey: "key", ServiceID: "cf-service",
				Client: clientFunc(func(request *http.Request) (*http.Response, error) {
					var body remoteRequest
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if request.URL.Path == "/api/turn/revoke" {
						if request.Context().Err() != nil || body != issuance {
							t.Fatal("cleanup lost its subject or inherited cancellation")
						}
						revoked = true
						return response(http.StatusAccepted, `{"state":"revocation_pending"}`), nil
					}
					issuance = body
					if invalid {
						return response(http.StatusCreated, `{"lease_id":"untrusted"}`), nil
					}
					cancel()
					return nil, ctx.Err()
				})})
			acquired, err := source.Issue(ctx, Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
			if err == nil || acquired.Credentials.Kind() != 0 || acquired.Cleanup == nil {
				t.Fatal(acquired, err)
			}
			result, err := acquired.Cleanup.Revoke(t.Context())
			if err != nil || result != RevocationQueued || !revoked {
				t.Fatal(result, err)
			}
		})
	}
}

func TestRemoteRetriesOnlyUncertainOrPendingResultsWithinOriginalDeadline(t *testing.T) {
	for _, code := range []string{"lease_pending", "lease_unavailable"} {
		t.Run(code, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				var first remoteRequest
				source, _ := NewRemote(RemoteConfig{RelayURL: "https://relay.example", AccessKey: "key", ServiceID: "cf-service",
					Client: clientFunc(func(request *http.Request) (*http.Response, error) {
						calls++
						var body remoteRequest
						_ = json.NewDecoder(request.Body).Decode(&body)
						if calls == 1 {
							first = body
						} else if body != first {
							t.Fatal("pending retry changed identity")
						}
						return response(http.StatusConflict, `{"error":"`+code+`"}`), nil
					})})
				acquired, err := source.Issue(t.Context(), Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
				want := 1
				if code == "lease_pending" {
					want = remoteIssueAttempts
				}
				if err == nil || acquired.Cleanup == nil || calls != want {
					t.Fatal(acquired, err, calls)
				}
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		source, _ := NewRemote(RemoteConfig{RelayURL: "https://relay.example", AccessKey: "key", ServiceID: "cf-service",
			Client: clientFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				<-request.Context().Done()
				return nil, request.Context().Err()
			})})
		start := time.Now()
		acquired, err := source.Issue(t.Context(), Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
		if !errors.Is(err, context.DeadlineExceeded) || acquired.Cleanup == nil || calls != 1 || time.Since(start) != HTTPTimeout {
			t.Fatal(acquired, err, calls, time.Since(start))
		}
	})
}

func TestRemoteCleanupDistinguishesTerminalAndTransferredResponsibility(t *testing.T) {
	for state, want := range map[string]CleanupResult{
		"revoked": RevocationConfirmed, "revocation_pending": RevocationQueued,
		"canceled": IssuanceCanceled, "issuance_failed": IssuanceCanceled, "expired": AuthorityExpired,
	} {
		t.Run(state, func(t *testing.T) {
			source, _ := NewRemote(RemoteConfig{RelayURL: "https://relay.example", AccessKey: "key", ServiceID: "cf-service",
				Client: clientFunc(func(*http.Request) (*http.Response, error) {
					return response(http.StatusOK, `{"state":"`+state+`"}`), nil
				})})
			result, err := source.revoke(t.Context(), source.request(Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}}, "lease"))
			if err != nil || result != want {
				t.Fatal(result, err)
			}
		})
	}
}
