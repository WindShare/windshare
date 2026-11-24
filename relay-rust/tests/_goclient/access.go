package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/relaysetup"
	"github.com/windshare/windshare/engine/turnissuer"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type accessAdmin struct {
	origin string
	cookie string
}
type managedCredential struct {
	AccessKey  string `json:"access_key"`
	Credential struct {
		ID string `json:"credential_id"`
	} `json:"credential"`
}

func (admin *accessAdmin) call(path string, body any, out any) {
	encoded, err := json.Marshal(body)
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, admin.origin+"/api/"+path, bytes.NewReader(encoded))
	check(err)
	req.Header.Set("Origin", admin.origin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", admin.cookie)
	response, err := http.DefaultClient.Do(req)
	check(err)
	defer response.Body.Close()
	require(response.StatusCode == 200 || response.StatusCode == 201, fmt.Sprintf("access %s: HTTP %d", path, response.StatusCode))
	for _, cookie := range response.Cookies() {
		admin.cookie = cookie.Name + "=" + cookie.Value
	}
	if out != nil {
		check(json.NewDecoder(response.Body).Decode(out))
	}
}
func (admin *accessAdmin) create(name string) managedCredential {
	var credential managedCredential
	admin.call("credentials", map[string]string{"name": name}, &credential)
	return credential
}
func (admin *accessAdmin) revoke(id string, terminate bool) {
	admin.call("credentials/revoke", map[string]any{"credential_id": id, "terminate_shares": terminate}, nil)
}

func accessScenario(endpoint, console string) {
	admin := accessAdmin{origin: console}
	admin.call("login", map[string]string{"password": os.Getenv("WINDSHARE_OWNER_PASSWORD")}, nil)
	directory, err := os.MkdirTemp("", "windshare-access-client-")
	check(err)
	path := filepath.Join(directory, "configuration.json")
	defer func() {
		_ = os.Remove(path)
		_ = os.Remove(path + ".lock")
		_ = os.Remove(filepath.Join(path+".state", "setup"))
		_ = os.Remove(path + ".state")
		_ = os.Remove(directory)
	}()
	baseURL := strings.TrimSuffix(endpoint, v2.WebSocketPath)
	store := relaysetup.FileStore{Settings: relayconfig.FileStore{Path: path}}
	service := relaysetup.New(relaysetup.Dependencies{Store: store})
	installed, err := service.Run(context.Background(), relaysetup.Request{
		URL:        baseURL,
		DeviceName: "Enrolled device",
		Operation:  relaysetup.AuthorizeDevice,
	}, func(progress relaysetup.Progress) {
		if progress.Stage == "approval_required" {
			admin.call("enrollments/approve", map[string]string{"enrollment_id": progress.EnrollmentID, "user_code": progress.UserCode}, nil)
		}
	})
	check(err)
	saved, err := store.Load()
	check(err)
	require(saved.Pending == nil && len(saved.Profiles) == 1 && len(saved.ActiveRelays) == 1, "automatic setup did not commit private selection")
	require(saved.Profiles[0].ManagedTurn, "automatic setup did not record advertised managed TURN capability")
	first := managedCredential{AccessKey: saved.Profiles[0].AccessKey}
	first.Credential.ID = installed.CredentialID
	verifyManagedTURN(baseURL, first.AccessKey, &admin)
	second := admin.create("Independent device")
	check(os.Setenv("WINDSHARE_RELAY_ACCESS_KEY", first.AccessKey))
	// A routed socket may not attribute a different share to its credential.
	// Enough mismatches to fill an alarm batch used to starve the real shares.
	for range 32 {
		routed, unrelated := newFixture(), newFixture()
		socket, _ := dial(endpoint, routed.init.Registration.ShareID)
		send(socket, unrelated.init)
		refusal(socket, v2.ErrorAccessDenied)
		check(socket.CloseNow())
	}
	f, stopped := newFixture(), newFixture()
	sender := register(endpoint, f, false)
	defer sender.CloseNow()
	toStop := register(endpoint, stopped, false)
	defer toStop.CloseNow()
	// Admission must observe revocation even on a socket accepted while the key was active.
	unregistered := newFixture()
	staleSocket, _ := dial(endpoint, unregistered.init.Registration.ShareID)
	defer staleSocket.CloseNow()
	admin.revoke(first.Credential.ID, false)
	send(staleSocket, unregistered.init)
	refusal(staleSocket, v2.ErrorAccessDenied)
	revokedTURN := issueManagedTURN(baseURL, first.AccessKey, true, p.TURNAll)
	require(revokedTURN.Status == p.TURNRetryLater && len(revokedTURN.Credentials) == 0, fmt.Sprintf("revoked credential unexpectedly issued managed TURN: status=%v count=%d", revokedTURN.Status, len(revokedTURN.Credentials)))
	probe(sender, 2)
	stop(endpoint, stopped)
	check(sender.CloseNow())
	sender = register(endpoint, f, true)
	defer sender.CloseNow()
	probe(sender, 3)

	check(os.Setenv("WINDSHARE_RELAY_ACCESS_KEY", second.AccessKey))
	independent := newFixture()
	other := register(endpoint, independent, false)
	defer other.CloseNow()
	admin.revoke(first.Credential.ID, true)
	// Administrative termination runs through the durable alarm queue.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		_, _, err := sender.Read(ctx)
		if err != nil {
			require(ctx.Err() == nil, "administrative termination did not settle")
			break
		}
	}
	probe(other, 4)
	resumed, _ := dialWithCredential(endpoint, f.init.Registration.ShareID, false)
	defer resumed.CloseNow()
	init := f.init
	init.Registration.Mode = v2.RegistrationResume
	send(resumed, init)
	send(resumed, v2.ResumeCredential{Token: f.token})
	refusal(resumed, v2.ErrorStopped)
	stop(endpoint, independent)
	fmt.Println("PASS: fresh admission rechecks revocation, managed TURN discovery/issuance/pause match Worker, RESUME and STOP retain authority, administrative termination is durable and credential-scoped")
}

func issueManagedTURN(baseURL, accessKey string, advertised bool, mode p.TURNMode) turngrant.IssueResult {
	candidate := relayprofile.Candidate{PublicURL: baseURL, AccessKey: accessKey, ManagedTurn: advertised}
	sources, err := turnissuer.SelectSources(nil, []relayprofile.Registration{candidate.Registration()}, []relayprofile.Candidate{candidate})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	resolver := sources.Open(ctx, turnissuer.Options{})
	defer resolver.Close()
	return resolver.Issue(ctx, turngrant.IssueRequest{Mode: mode})
}

func verifyManagedTURN(baseURL, accessKey string, admin *accessAdmin) {
	// Parallel demand must leave managed fallback untouched.
	parallel := issueManagedTURN(baseURL, accessKey, true, p.TURNParallel)
	require(parallel.Status == p.TURNOK && parallel.HasFallback && len(parallel.Credentials) == 0, "parallel demand unexpectedly issued managed fallback credentials")

	// Start without cached capability so Resolver performs live /.well-known/windshare discovery.
	issued := issueManagedTURN(baseURL, accessKey, false, p.TURNAll)
	require(issued.Status == p.TURNOK && issued.HasFallback && len(issued.Credentials) == 1, fmt.Sprintf("managed TURN issuance failed: status=%v fallback=%v count=%d", issued.Status, issued.HasFallback, len(issued.Credentials)))
	credential := issued.Credentials[0]
	require(slices.Equal(credential.URLs, []string{"turn:127.0.0.1:3478?transport=udp"}), fmt.Sprintf("unexpected managed TURN URLs: %v", credential.URLs))
	require(credential.Username == "ephemeral-user" && credential.Password == "ephemeral-pass", "unexpected managed TURN credentials")
	remaining := time.Until(credential.ExpiresAt)
	require(remaining > 25*time.Minute && remaining <= 30*time.Minute, fmt.Sprintf("unexpected managed TURN expiry window: %s", remaining))

	// Pausing managed TURN must surface turn_disabled and cache the share-lifetime disable decision.
	admin.call("turn/toggle", map[string]any{"enabled": false}, nil)
	defer admin.call("turn/toggle", map[string]any{"enabled": true}, nil)
	candidate := relayprofile.Candidate{PublicURL: baseURL, AccessKey: accessKey, ManagedTurn: true}
	sources, err := turnissuer.SelectSources(nil, []relayprofile.Registration{candidate.Registration()}, []relayprofile.Candidate{candidate})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	resolver := sources.Open(ctx, turnissuer.Options{})
	defer resolver.Close()
	paused := resolver.Issue(ctx, turngrant.IssueRequest{Mode: p.TURNAll})
	require(paused.Status == p.TURNUnconfigured && !paused.HasFallback && len(paused.Credentials) == 0, fmt.Sprintf("paused managed TURN did not disable cleanly: status=%v fallback=%v", paused.Status, paused.HasFallback))
	cached := resolver.Issue(ctx, turngrant.IssueRequest{Mode: p.TURNAll})
	require(cached.Status == p.TURNUnconfigured && len(cached.Credentials) == 0, "cached managed TURN disable did not persist")
}
