package account

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/serviceauthority"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestAnonymousAttributionWaitsForEveryRegistrationOutcome(t *testing.T) {
	const unsent = serviceauthority.RegistrationNotSent
	const ambiguous = serviceauthority.RegistrationMayHaveBeenSent
	for _, test := range []struct {
		name          string
		first, second serviceauthority.RegistrationDelivery
		wantAccount   bool
	}{
		{name: "both unsent", first: unsent, second: unsent, wantAccount: true},
		{name: "first may have registered", first: ambiguous, second: unsent},
		{name: "second may have registered", first: unsent, second: ambiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, ticket := accountFixture(t)
			service := testService(t, config)
			share := service.NewShareAuthorization(t.Context())
			defer share.Close()
			for _, address := range []string{"primary", "secondary"} {
				service.nodes.put(address, &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
			}
			first := reserveBearer(t, &Source{share: share, address: "primary"}, "")
			if err := service.install(t.Context(), Record{}, ticket, true); err != nil {
				t.Fatal(err)
			}
			second := reserveBearer(t, &Source{share: share, address: "secondary"}, "")
			backup := &Source{share: share, address: "account-only"}
			wait := backup.ReserveRegistrationAuthorization()
			if wait.State != serviceauthority.AdmissionPendingAttribution || wait.Reservation != nil {
				t.Fatalf("account-only node acquired authorization during anonymous registration: %s", wait.State.Name())
			}

			first.Reservation.Finish(test.first)
			if test.first == unsent {
				first.Reservation.Finish(unsent)
				select {
				case <-wait.Changed:
					t.Fatal("an anonymous registration was still in flight")
				default:
				}
			}
			second.Reservation.Finish(test.second)
			select {
			case <-wait.Changed:
			default:
				t.Fatal("resolving attribution did not wake the backup")
			}
			wantBearer := ""
			if test.wantAccount {
				wantBearer = ticket.Ticket
			}
			requireBearer(t, backup, wantBearer)
		})
	}
}

func TestFirstShareUsesNewTicketAfterAnonymousUpgradeFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	config, ticket := accountFixture(t)
	service := testService(t, config)

	upgrade := make(chan struct{}, 1)
	failUpgrade := make(chan struct{})
	anonymous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			select {
			case upgrade <- struct{}{}:
			default:
			}
		}
		select {
		case <-failUpgrade:
			http.NotFound(w, r)
		case <-ctx.Done():
		}
	}))
	t.Cleanup(anonymous.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveTestAccountRegistration(t, ctx, w, r, ticket.Ticket)
	}))
	t.Cleanup(backup.Close)

	service.nodes.put(anonymous.URL, &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
	share := service.NewShareAuthorization(ctx)
	defer share.Close()
	registration := testRegistrationConfig(t)
	admissions := relayset.NewNetworkAdmissions(relayplan.Plan{Network: []relayplan.Candidate{
		{URL: anonymous.URL}, {URL: backup.URL},
	}}, nil, nil)
	waiting := make(chan struct{}, 1)
	set, err := relayset.NewSender(ctx, []string{anonymous.URL, backup.URL}, func(_ context.Context, address string) (relayset.SenderEndpoint, error) {
		candidate := registration
		candidate.Candidate = relayset.Candidate{PublicURL: address, Authorization: &Source{share: share, address: address}}
		candidate.Attempts = admissions.Control(address)
		candidate.ObserveAttempt = func(attempt relayset.Attempt) {
			if address == backup.URL && attempt.State == relayset.AttemptAuthorization &&
				attempt.Authorization == relayset.AuthorizationPendingAttribution {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
		}
		return relayset.NewSenderLifecycle(candidate)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.StopRecovery()
	for _, stage := range []<-chan struct{}{upgrade, waiting} {
		select {
		case <-stage:
		case <-ctx.Done():
			t.Fatalf("anonymous upgrade or waiting account fallback was not reached: %v", ctx.Err())
		}
	}
	if err := service.install(ctx, Record{}, ticket, true); err != nil {
		t.Fatal(err)
	}
	close(failUpgrade)
	if err := set.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if set.ReadyRelayURL() != backup.URL {
		t.Fatal("the account relay did not publish the original share")
	}
}

func TestAccountReservationNotSentReleasesTentativeCredential(t *testing.T) {
	config, first := accountFixture(t)
	second := otherAccount(t, config, first)
	service := testService(t, config)

	// Share created before any account exists has no pre-bound credential.
	unbound := service.NewShareAuthorization(t.Context())
	defer unbound.Close()
	if err := service.install(t.Context(), Record{}, first, true); err != nil {
		t.Fatal(err)
	}
	// Share created after login explicitly owns the pre-bound credential.
	prebound := service.NewShareAuthorization(t.Context())
	defer prebound.Close()

	firstAttempt := reserveBearer(t, &Source{share: unbound, address: "primary"}, first.Ticket)
	secondAttempt := reserveBearer(t, &Source{share: unbound, address: "secondary"}, first.Ticket)
	firstAttempt.Reservation.Finish(serviceauthority.RegistrationNotSent)
	service.mu.Lock()
	stillRetained := unbound.credential != nil
	service.mu.Unlock()
	if !stillRetained {
		t.Fatal("released tentative credential while another attempt was still in flight")
	}
	secondAttempt.Reservation.Finish(serviceauthority.RegistrationNotSent)
	service.mu.Lock()
	releasedAfterAllNotSent := unbound.credential == nil
	service.mu.Unlock()
	if !releasedAfterAllNotSent {
		t.Fatal("tentative credential stayed retained after all unsent reservations rolled back")
	}

	// Pre-bound shares keep their selected credential across unsent attempts.
	preboundAttempt := reserveBearer(t, &Source{share: prebound, address: "primary"}, first.Ticket)
	preboundAttempt.Reservation.Finish(serviceauthority.RegistrationNotSent)
	service.mu.Lock()
	preboundKept := prebound.credential != nil
	service.mu.Unlock()
	if !preboundKept {
		t.Fatal("pre-bound share lost its selected credential on unsent attempt")
	}

	if err := service.install(t.Context(), cachedRecord(first), second, true); err != nil {
		t.Fatal(err)
	}
	requireBearer(t, &Source{share: unbound, address: "primary"}, second.Ticket)
	requireBearer(t, &Source{share: prebound, address: "primary"}, first.Ticket)
}

func testRegistrationConfig(t *testing.T) relayset.SenderLifecycleConfig {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := link.NewSenderAuthenticated(make([]byte, link.ReadSecretBytes), public, nil)
	if err != nil {
		t.Fatal(err)
	}
	rawID, err := base64.RawURLEncoding.DecodeString(capability.ShareID)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, token := []byte("test publication"), v2.ResumeToken{1}
	fresh, err := relayv2.NewFreshRegisterInit(v2.ShareID(rawID), v2.ShareInstance{1}, v2.PKHash(capability.PKHash), descriptor, token)
	if err != nil {
		t.Fatal(err)
	}
	return relayset.SenderLifecycleConfig{Fresh: fresh, PrivateKey: private, Descriptor: descriptor, ResumeToken: token}
}

func serveTestAccountRegistration(t *testing.T, ctx context.Context, w http.ResponseWriter, r *http.Request, ticket string) {
	t.Helper()
	socket, err := websocket.Accept(w, r, nil)
	if err != nil {
		t.Error(err)
		return
	}
	defer socket.CloseNow()
	_, encoded, err := socket.Read(ctx)
	if err != nil {
		return
	}
	init, err := v2.ParseDeliveryRegisterInit(encoded)
	if err != nil {
		t.Error(err)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+ticket {
		denied, _ := (v2.ErrorFrame{Code: v2.ErrorAccessDenied}).MarshalBinary()
		_ = socket.Write(ctx, websocket.MessageBinary, denied)
		return
	}

	challenge, _ := (v2.Challenge{
		Purpose: v2.ChallengeRegister, ID: v2.ChallengeID{1}, Nonce: v2.ChallengeNonce{1},
		ExpiresAtUnixSeconds: uint64(time.Now().Add(time.Minute).Unix()),
	}).MarshalBinary()
	if err := socket.Write(ctx, websocket.MessageBinary, challenge); err != nil {
		return
	}
	for range 2 {
		if _, _, err := socket.Read(ctx); err != nil {
			return
		}
	}
	registered, _ := (v2.Registered{
		ShareID: init.Registration.ShareID, ShareInstance: init.Registration.ShareInstance,
		DescriptorDigest: init.Registration.DescriptorDigest,
	}).MarshalBinary()
	if err := socket.Write(ctx, websocket.MessageBinary, registered); err == nil {
		<-ctx.Done()
	}
}
