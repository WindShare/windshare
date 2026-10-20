package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	defer func() { _ = os.Remove(path); _ = os.Remove(path + ".lock"); _ = os.Remove(directory) }()
	store := relayconfig.FileStore{Path: path}
	service := relaysetup.New(relaysetup.Dependencies{Store: store})
	installed, err := service.Run(context.Background(), relaysetup.Request{
		URL: strings.TrimSuffix(endpoint, v2.WebSocketPath), DeviceName: "Enrolled device", Operation: relaysetup.PrivateOnboarding,
	}, func(progress relaysetup.Progress) {
		if progress.Stage == "approval_required" {
			admin.call("enrollments/approve", map[string]string{"enrollment_id": progress.EnrollmentID, "user_code": progress.UserCode}, nil)
		}
	})
	check(err)
	saved, err := store.Load()
	check(err)
	require(saved.Pending == nil && len(saved.Profiles) == 1 && len(saved.ActiveRelays) == 1, "automatic setup did not commit private selection")
	first := managedCredential{AccessKey: saved.Profiles[0].AccessKey}
	first.Credential.ID = installed.CredentialID
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
	fmt.Println("PASS: fresh admission rechecks revocation, RESUME and STOP retain authority, administrative termination is durable and credential-scoped")
}
