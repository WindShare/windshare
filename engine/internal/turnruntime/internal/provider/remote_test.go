package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRemoteLeaseBindsRenewalAndRevocationToSubject(t *testing.T) {
	now := time.Unix(1700000000, 0)
	subject := Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}}
	var requests []remoteRequest
	client := clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer device-key" || request.URL.Host != "relay.example" {
			t.Fatal("device authority escaped its deployment")
		}
		if request.URL.Path == "/api/turn/capability" {
			return response(200, `{"turn":{"service_id":"cf-service","enabled":true}}`), nil
		}
		var body remoteRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		if request.URL.Path == "/api/turn/revoke" {
			return response(202, `{"state":"revocation_pending"}`), nil
		}
		data, _ := json.Marshal(remoteGrant{ServiceID: body.ServiceID, LeaseID: body.LeaseID,
			SessionID: body.SessionID, PathID: body.PathID, URLs: []string{"turn:localhost:3478"},
			Username: "temporary-user", Password: "temporary-password", ExpiresAt: now.Add(time.Hour).UnixMilli()})
		reply := response(201, string(data))
		reply.Header.Set("Date", now.UTC().Format(http.TimeFormat))
		return reply, nil
	})
	capability, err := Discover(t.Context(), client, "https://relay.example", "device-key")
	if err != nil || capability.ServiceID != "cf-service" || !capability.Enabled {
		t.Fatal(capability, err)
	}
	source, err := NewRemote(RemoteConfig{RelayURL: "https://relay.example", AccessKey: "device-key", ServiceID: capability.ServiceID,
		Client: client, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.Issue(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	subject.PathID = [16]byte{3}
	second, err := source.Renew(t.Context(), subject, first)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Credentials.ExpiresAt().After(first.Credentials.ExpiresAt()) {
		t.Fatal("renewal failed to refresh local credential expiry")
	}
	if requests[1].PreviousLeaseID != first.Cleanup.ID || second.Cleanup.ID == first.Cleanup.ID {
		t.Fatal("renewal lost lease identity")
	}
	if outcome, err := first.Cleanup.Revoke(t.Context()); err != nil || outcome != RevocationQueued {
		t.Fatal(outcome, err)
	}
	if requests[2] != *first.remote {
		t.Fatal("revocation changed subject")
	}
	if _, err := source.Renew(t.Context(), Subject{SessionID: [16]byte{9}}, first); err == nil {
		t.Fatal("cross-session renewal accepted")
	}
}
func TestRemoteRejectsUnboundOrUnboundedAuthority(t *testing.T) {
	for _, mutate := range []func(*remoteGrant){
		func(g *remoteGrant) { g.ServiceID = "other" },
		func(g *remoteGrant) { g.SessionID = "other" },
		func(g *remoteGrant) { g.LeaseID = "other" },
		func(g *remoteGrant) { g.ExpiresAt = time.Now().Add(72 * time.Hour).UnixMilli() },
		func(g *remoteGrant) { g.ExpiresAt = 1 },
		func(g *remoteGrant) { g.URLs = []string{"https://unexpected.example"} },
	} {
		source, _ := NewRemote(RemoteConfig{RelayURL: "https://relay.example", AccessKey: "key", ServiceID: "cf-service",
			Client: clientFunc(func(request *http.Request) (*http.Response, error) {
				var body remoteRequest
				_ = json.NewDecoder(request.Body).Decode(&body)
				grant := remoteGrant{ServiceID: body.ServiceID, LeaseID: body.LeaseID, SessionID: body.SessionID,
					PathID: body.PathID, URLs: []string{"turn:localhost"}, Username: "u", Password: "p", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
				mutate(&grant)
				data, _ := json.Marshal(grant)
				return response(200, string(data)), nil
			})})
		if _, err := source.Issue(t.Context(), Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}}); err == nil {
			t.Fatal("invalid grant accepted")
		}
	}
	for _, address := range []string{"http://remote.example", "https://relay.example?token=value", "https://user@relay.example", "https://relay.example/path"} {
		if _, err := ManagedEndpoint(address); err == nil {
			t.Fatal(address)
		}
	}
}
func TestRemoteDoesNotFollowCredentialRedirect(t *testing.T) {
	const relayHost = "relay.example"
	requests := make(chan string, 2)
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Host:
		default:
			t.Error("unexpected extra redirect request")
		}
		if r.Host != relayHost {
			t.Error("credential request reached redirect target", r.Host)
			return
		}
		if r.Header.Get("Authorization") != "Bearer private-device-key" {
			t.Error("deployment credential was omitted")
		}
		http.Redirect(w, r, "https://other.example/api/turn/capability", http.StatusTemporaryRedirect)
	}))
	client := remoteHTTPClient()
	// Keep the credential policy while routing every host through the in-memory network.
	client.Transport = server.Client().Transport
	if _, err := Discover(t.Context(), client, "https://"+relayHost, "private-device-key"); err == nil {
		t.Fatal("redirect accepted")
	}
	if len(requests) != 1 || <-requests != relayHost {
		t.Fatal("credential was omitted or followed a redirect")
	}
}
