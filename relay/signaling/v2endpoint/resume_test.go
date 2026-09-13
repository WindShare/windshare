package v2endpoint

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
	"github.com/windshare/windshare/transport/relayv2"
)

const resumeTestRelayBase = "https://relay.example/resume-test"

func newResumeTestServer(t *testing.T) (*Server, endpointFixture) {
	t.Helper()
	return newResumeTestServerWithTracer(t, nil)
}

func newResumeTestServerWithTracer(t *testing.T, tracer v2route.ResumeTracer) (*Server, endpointFixture) {
	t.Helper()
	endpoint, err := v2.NormalizeRelayEndpoint(resumeTestRelayBase)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 4, MaxSessions: 16, MaxSessionsPerShare: 8,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{}, ResumeTracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 16, Random: &sequenceReader{next: 31},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Registry: registry, Challenges: ledger, RelayIdentity: endpoint.Identity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return server, newEndpointFixture(t)
}

func dialResumeTestSender(t *testing.T, server *Server, fixture endpointFixture, resume bool) *relayv2.SenderConnection {
	t.Helper()
	init := fixture.init
	if resume {
		init.Mode = v2.RegistrationResume
	}
	connection, err := relayv2.DialSender(context.Background(), relayv2.SenderConfig{
		RelayBaseURL: resumeTestRelayBase, Init: init, SenderPrivateKey: fixture.privateKey,
		Descriptor: fixture.descriptor, ResumeToken: fixture.token,
		Dial: relayv2.DialOptions{SocketDialer: memoryServerDialer(server)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func TestResumeTakesOverLiveSocketAndRetiresOnlyItsRelaySessions(t *testing.T) {
	server, fixture := newResumeTestServer(t)
	old := dialResumeTestSender(t, server, fixture, false)
	receiver := dialReceiver(t, resumeTestRelayBase, fixture.init.ShareID, memoryServerDialer(server))
	t.Cleanup(func() { _ = receiver.Close() })
	oldSession := establishSession(t, old, receiver, []byte("before-takeover"))
	assertFrame(t, oldSession.Recv(), "before-takeover")

	replacement := dialResumeTestSender(t, server, fixture, true)
	for name, done := range map[string]<-chan struct{}{"old sender": old.Done(), "old receiver": receiver.Done()} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s was not retired", name)
		}
	}
	joined := dialReceiver(t, resumeTestRelayBase, fixture.init.ShareID, memoryServerDialer(server))
	t.Cleanup(func() { _ = joined.Close() })
	if !bytes.Equal(joined.Descriptor(), fixture.descriptor) {
		t.Fatal("takeover changed descriptor bytes")
	}
	channel := establishSession(t, replacement, joined, []byte("after-takeover"))
	assertFrame(t, channel.Recv(), "after-takeover")
}

type dropRegisteredSocket struct {
	BinaryConnection
	dropped chan struct{}
}

func (socket *dropRegisteredSocket) Write(ctx context.Context, kind websocket.MessageType, encoded []byte) error {
	if bytes.HasPrefix(encoded, []byte("WS2K")) {
		close(socket.dropped)
		return nil
	}
	return socket.BinaryConnection.Write(ctx, kind, encoded)
}

func TestLostFreshConfirmationRecoversWhileOriginalSocketIsStillLive(t *testing.T) {
	server, fixture := newResumeTestServer(t)
	dropped := make(chan struct{})
	dial := func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
		client, relay := newMemorySocketPair()
		go func() {
			_ = server.Serve(context.Background(), &dropRegisteredSocket{BinaryConnection: relay, dropped: dropped})
		}()
		return client, nil
	}
	freshDone := make(chan error, 1)
	go func() {
		sender, err := relayv2.DialSender(context.Background(), relayv2.SenderConfig{
			RelayBaseURL: resumeTestRelayBase, Init: fixture.init, SenderPrivateKey: fixture.privateKey,
			Descriptor: fixture.descriptor, Dial: relayv2.DialOptions{SocketDialer: dial},
		})
		if sender != nil {
			_ = sender.Close()
		}
		freshDone <- err
	}()
	<-dropped
	replacement := dialResumeTestSender(t, server, fixture, true)
	select {
	case err := <-freshDone:
		if err == nil {
			t.Fatal("lost confirmation falsely succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("takeover did not close unconfirmed original connection")
	}
	receiver := dialReceiver(t, resumeTestRelayBase, fixture.init.ShareID, memoryServerDialer(server))
	t.Cleanup(func() { _ = receiver.Close() })
	channel := establishSession(t, replacement, receiver, []byte("after-lost-confirmation"))
	assertFrame(t, channel.Recv(), "after-lost-confirmation")
}

func writeResumeTestFrame(t *testing.T, socket BinaryConnection, frame interface{ MarshalBinary() ([]byte, error) }) {
	t.Helper()
	encoded, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := socket.Write(ctx, websocket.MessageBinary, encoded); err != nil {
		t.Fatal(err)
	}
}

func readResumeTestFrame(t *testing.T, socket BinaryConnection) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	encoded, err := readBinary(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func startResumeTestWire(t *testing.T, server *Server, fixture endpointFixture) (*memorySocket, v2.RegisterInit) {
	t.Helper()
	client, relay := newMemorySocketPair()
	t.Cleanup(func() { _ = client.Close(websocket.StatusNormalClosure, "") })
	go func() { _ = server.Serve(context.Background(), relay) }()
	init := fixture.init
	init.Mode = v2.RegistrationResume
	writeResumeTestFrame(t, client, init)
	writeResumeTestFrame(t, client, v2.ResumeCredential{Token: fixture.token})
	return client, init
}

func proveResumeTestWire(t *testing.T, server *Server, client BinaryConnection, fixture endpointFixture, init v2.RegisterInit, challenge v2.Challenge) {
	t.Helper()
	proof, err := v2.NewRegisterProof(init, challenge, server.relayIdentity, fixture.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	writeResumeTestFrame(t, client, proof)
}

func TestResumeWireRejectsLateChallengeCommitAsStale(t *testing.T) {
	server, fixture := newResumeTestServer(t)
	dialResumeTestSender(t, server, fixture, false)
	first, init := startResumeTestWire(t, server, fixture)
	firstChallenge, err := v2.ParseChallenge(readResumeTestFrame(t, first))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := startResumeTestWire(t, server, fixture)
	secondChallenge, err := v2.ParseChallenge(readResumeTestFrame(t, second))
	if err != nil {
		t.Fatal(err)
	}
	proveResumeTestWire(t, server, first, fixture, init, firstChallenge)
	if _, err := v2.ParseRegistered(readResumeTestFrame(t, first)); err != nil {
		t.Fatal(err)
	}
	proveResumeTestWire(t, server, second, fixture, init, secondChallenge)
	failure, err := v2.ParseError(readResumeTestFrame(t, second))
	if err != nil || failure.Code != v2.ErrorResumeStale {
		t.Fatalf("late commit = %+v, %v", failure, err)
	}
}

func TestResumeWireAuthenticatesAbsenceAndDistinguishesUnpublishedRoute(t *testing.T) {
	server, fixture := newResumeTestServer(t)
	client, init := startResumeTestWire(t, server, fixture)
	challenge, err := v2.ParseChallenge(readResumeTestFrame(t, client))
	if err != nil {
		t.Fatalf("absence was exposed before authentication: %v", err)
	}
	proveResumeTestWire(t, server, client, fixture, init, challenge)
	missing, err := v2.ParseError(readResumeTestFrame(t, client))
	if err != nil || missing.Code != v2.ErrorNotFound {
		t.Fatalf("authenticated absence = %+v, %v", missing, err)
	}

	if err := server.registry.BeginRegistration(fixture.init, endpointTestConnectionRef("still-uploading")); err != nil {
		t.Fatal(err)
	}
	starting, _ := startResumeTestWire(t, server, fixture)
	retry, err := v2.ParseError(readResumeTestFrame(t, starting))
	if err != nil || retry.Code != v2.ErrorStarting || retry.RetryAfter != registrationRetryDelay {
		t.Fatalf("unpublished resume = %+v, %v", retry, err)
	}
	invalid := fixture
	invalid.token[0] ^= 1
	wrong, _ := startResumeTestWire(t, server, invalid)
	rejected, err := v2.ParseError(readResumeTestFrame(t, wrong))
	if err != nil || rejected.Code != v2.ErrorInvalidProof {
		t.Fatalf("invalid token = %+v, %v", rejected, err)
	}
}

func TestResumeWireFailureDoesNotMisclassifyStaleAsIdentityConflict(t *testing.T) {
	for cause, want := range map[error]v2.ErrorCode{
		v2route.ErrStarting: v2.ErrorStarting, v2route.ErrResumeStale: v2.ErrorResumeStale,
		v2route.ErrResume: v2.ErrorInvalidProof, v2route.ErrNotFound: v2.ErrorNotFound,
		v2route.ErrStopping: v2.ErrorAdmission, v2route.ErrStopped: v2.ErrorStopped,
	} {
		if got := registryErrorCode(errors.Join(cause, errors.New("context"))); got != want {
			t.Fatalf("%v mapped to %d, want %d", cause, got, want)
		}
	}
}
