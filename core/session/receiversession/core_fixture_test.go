package receiversession

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type coreFixture struct {
	keys     protocolsession.SessionKeys
	channel  *memoryChannel
	peer     *memoryChannel
	recorder *protocolTraceRecorder
	started  bool
	auth     *switchableAuthenticator
}

var coreFixtures sync.Map

func testCore(runtime *sessioncore.Runtime) *coreFixture {
	fixture, ok := coreFixtures.Load(runtime)
	if !ok {
		panic("runtime has no protocol test fixture")
	}
	return fixture.(*coreFixture)
}
func startReceiverPeerWriter(t *testing.T, runtime *sessioncore.Runtime) {
	t.Helper()
	fixture := testCore(runtime)
	if fixture.started {
		return
	}
	fixture.started = true
	runtime.Start()
	t.Cleanup(runtime.Close)
}
func receiverRequestContext(call *sessioncore.Call) context.Context {
	generation, _ := call.OperationAuthority()
	return protocolsession.WithOperationGeneration(context.Background(), generation)
}

func runtimeIdentity(runtime interface{ LaneIdentity() (uint32, uint32) }) session.LaneIdentity {
	id, epoch := runtime.LaneIdentity()
	return session.LaneIdentity{ID: id, Epoch: epoch}
}

type switchableReader struct {
	mu     sync.Mutex
	reader io.Reader
	err    error
}

func (reader *switchableReader) Read(p []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.err != nil {
		return 0, reader.err
	}
	return reader.reader.Read(p)
}
func (reader *switchableReader) fail(err error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.err = err
}

type switchableAuthenticator struct {
	mu      sync.Mutex
	current protocolsession.InboundMessageAuthenticator
}

func (auth *switchableAuthenticator) AuthenticateInbound(sequence uint64, message protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
	auth.mu.Lock()
	current := auth.current
	auth.mu.Unlock()
	return current.AuthenticateInbound(sequence, message)
}
func sendAuthenticatedTestMessage(t *testing.T, runtime *sessioncore.Runtime, message protocolsession.Message, auth protocolsession.InboundMessageAuthenticator) {
	t.Helper()
	fixture := testCore(runtime)
	fixture.auth.mu.Lock()
	fixture.auth.current = auth
	fixture.auth.mu.Unlock()
	lane := runtimeIdentity(runtime)
	sealer, err := protocolsession.NewEnvelopeSealer(fixture.keys.SenderToReceiver(), protocolsession.EnvelopeBinding{
		ShareInstance: runtime.ShareInstance(), ProtocolSessionID: runtime.ProtocolSessionID(), LaneID: lane.ID, LaneEpoch: lane.Epoch, Direction: protocolsession.DirectionSenderToReceiver}, &deterministicReader{next: 137})
	if err != nil {
		t.Fatal(err)
	}
	defer sealer.Destroy()
	plaintext, err := protocolsession.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.peer.Send(context.Background(), sealed.Frame); err != nil {
		t.Fatal(err)
	}
}

func stopTestCore(runtime *sessioncore.Runtime) {
	fixture, ok := coreFixtures.Load(runtime)
	if ok && fixture.(*coreFixture).started {
		runtime.Close()
	} else {
		runtime.AbortBeforeStart()
	}
}
func sendTestMessageOnLane(t *testing.T, runtime *sessioncore.Runtime, lane session.LaneIdentity, message protocolsession.Message) {
	t.Helper()
	fixture := testCore(runtime)
	peer := fixture.peer
	if lane != runtimeIdentity(runtime) {
		channel, other := newMemoryChannelPair()
		peer = other
		if err := runtime.InstallLane(lane, channel, permissiveInboundAuthenticator(), nil, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Close() })
	}
	sealer, err := protocolsession.NewEnvelopeSealer(fixture.keys.SenderToReceiver(), protocolsession.EnvelopeBinding{
		ShareInstance: runtime.ShareInstance(), ProtocolSessionID: runtime.ProtocolSessionID(), LaneID: lane.ID, LaneEpoch: lane.Epoch, Direction: protocolsession.DirectionSenderToReceiver}, &deterministicReader{next: 137})
	if err != nil {
		t.Fatal(err)
	}
	defer sealer.Destroy()
	plaintext, err := protocolsession.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(context.Background(), sealed.Frame); err != nil {
		t.Fatal(err)
	}
}
