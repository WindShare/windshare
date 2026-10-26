package receiversession

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

var requestFixtures sync.Map

// The spy records handles returned to this consumer. It never reaches into the
// core's operation table or response queues, so tests exercise the same boundary.
type trackedRequestClient struct {
	*sessioncore.Requests
	mu         sync.Mutex
	calls      map[protocolsession.OperationID]*sessioncore.Call
	closed     bool
	cancelGate <-chan struct{}
	cancelErr  error
}

func newTestRequests(runtime *sessioncore.Runtime, random io.Reader) *trackedRequestClient {
	return &trackedRequestClient{Requests: sessioncore.NewRequests(runtime, random), calls: make(map[protocolsession.OperationID]*sessioncore.Call)}
}
func trackReceiverFactory(factory *Factory) {
	factory.requests = func(runtime *sessioncore.Runtime, random io.Reader) requestClient {
		return newTestRequests(runtime, random)
	}
}
func trackedRequests(client requestClient) *trackedRequestClient {
	return client.(*trackedRequestClient)
}
func (client *trackedRequestClient) remember(call *sessioncore.Call) {
	if call == nil {
		return
	}
	client.mu.Lock()
	client.calls[call.ID()] = call
	client.mu.Unlock()
	requestFixtures.Store(call, client)
}
func (client *trackedRequestClient) Begin(ctx context.Context, kind protocolsession.MessageKind, body []byte) (*sessioncore.Call, error) {
	call, err := client.Requests.Begin(ctx, kind, body)
	client.remember(call)
	return call, err
}
func (client *trackedRequestClient) PrepareCall(ctx context.Context, kind protocolsession.MessageKind, body []byte) (*sessioncore.Call, protocolsession.Message, error) {
	call, message, err := client.Requests.PrepareCall(ctx, kind, body)
	client.remember(call)
	return call, message, err
}
func (client *trackedRequestClient) BeginOn(ctx context.Context, lane *sessioncontract.LaneIdentity, kind protocolsession.MessageKind, body []byte) (*sessioncore.Call, error) {
	call, message, err := client.PrepareCall(ctx, kind, body)
	if err != nil {
		return nil, err
	}
	if err = client.BeginCall(ctx, lane, call, message); err != nil {
		client.forget(call)
		return nil, err
	}
	return call, nil
}
func (client *trackedRequestClient) forget(call *sessioncore.Call) {
	if call == nil {
		return
	}
	client.mu.Lock()
	if client.calls[call.ID()] == call {
		delete(client.calls, call.ID())
	}
	client.mu.Unlock()
}
func (client *trackedRequestClient) End(call *sessioncore.Call) {
	client.Requests.End(call)
	client.forget(call)
}
func (client *trackedRequestClient) CancelAndEnd(call *sessioncore.Call, reason contentflow.CancelReason) error {
	if client.cancelGate != nil {
		<-client.cancelGate
	}
	call.RecordTraceCleanup(client.cancelErr)
	err := errors.Join(client.Requests.CancelAndEnd(call, reason), client.cancelErr)
	client.forget(call)
	return err
}
func (client *trackedRequestClient) Close() {
	client.Requests.Close()
	client.mu.Lock()
	for _, call := range client.calls {
		requestFixtures.Delete(call)
	}
	clear(client.calls)
	client.closed = true
	client.mu.Unlock()
}
func onlyActiveCall(t *testing.T, rpc requestClient) *sessioncore.Call {
	t.Helper()
	client := trackedRequests(rpc)
	client.mu.Lock()
	defer client.mu.Unlock()
	var active *sessioncore.Call
	for _, call := range client.calls {

		if active != nil {
			t.Fatal("multiple active consumer request handles")
		}
		active = call
	}
	if active == nil {
		t.Fatal("no active consumer request handle")
	}
	return active
}
func enqueueCallResponse(call *sessioncore.Call, message protocolsession.Message) {
	if err := deliverCallResponse(call, message); err != nil {
		panic(err)
	}
}
func deliverCallResponse(call *sessioncore.Call, message protocolsession.Message) error {
	client, ok := requestFixtures.Load(call)
	if !ok {
		return sessioncontract.ErrOperationMissing
	}
	generation, _ := call.OperationAuthority()
	return client.(*trackedRequestClient).HandleMessage(protocolsession.WithOperationGeneration(context.Background(), generation), message)
}

func assertRequestsRetired(t *testing.T, rpc requestClient) {
	t.Helper()
	client := trackedRequests(rpc)
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, call := range client.calls {
		generation, authority := call.OperationAuthority()
		if !generation.IsZero() || !authority.IsZero() {
			t.Fatalf("request %x retained operation authority", call.ID())
		}
	}
}

func cancelPendingCall(t *testing.T, rpc requestClient, call *sessioncore.Call) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rpc.AwaitCall(ctx, call, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel pending call: %v", err)
	}
}

func (client *trackedRequestClient) SendContinuation(ctx context.Context, call *sessioncore.Call, kind protocolsession.MessageKind, body []byte) (protocolsession.SendOutcome, error) {
	if kind == protocolsession.MessageCancel && client.cancelGate != nil {
		<-client.cancelGate
	}
	return client.Requests.SendContinuation(ctx, call, kind, body)
}
