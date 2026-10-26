package sessioncore

import (
	"context"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"io"
	"time"
)

func NewLockedReader(reader io.Reader) *LockedReader { return &LockedReader{reader: reader} }
func HandOff(channel protocolsession.FrameChannel, receive <-chan framechannel.Frame) *HandedOffChannel {
	return &HandedOffChannel{FrameChannel: channel, receive: receive}
}
func (runtime *Runtime) ShareInstance() catalog.ShareInstance { return runtime.share }
func (runtime *Runtime) Now() time.Time                       { return runtime.now() }
func (runtime *Runtime) AbortBeforeStart()                    { runtime.abortBeforeStart() }
func (runtime *Runtime) AddFinalizer(finalize func()) error   { return runtime.addFinalizer(finalize) }
func (runtime *Runtime) BeginExternalAdmission(ctx context.Context) (context.Context, func(), error) {
	return runtime.beginExternalAdmission(ctx)
}
func (runtime *Runtime) RegisterHandler(kind protocolsession.MessageKind, handler protocolsession.MessageHandler) error {
	return runtime.router.RegisterHandler(kind, handler)
}
func (runtime *Runtime) Start(components ...Component) { runtime.start(components...) }
func (runtime *Runtime) RecordError(err error)         { runtime.recordError(err) }
func (runtime *Runtime) Fail(err error)                { runtime.terminateRuntimeFailed(err) }
func (runtime *Runtime) FailProtocol(err error) {
	_ = runtime.router.TerminateLocal()
	runtime.terminateRuntimeFailed(err)
}
func (runtime *Runtime) FailOperationAuthority() error { return runtime.failRPCOperationAuthority() }
func (runtime *Runtime) BeginClose()                   { runtime.beginClose() }
func (runtime *Runtime) WaitClosed()                   { runtime.waitClosed() }
func (runtime *Runtime) ClaimTermination(cause TerminationCause) TerminationClaim {
	return runtime.claimTermination(cause)
}
func (runtime *Runtime) CancelLifetime() { runtime.cancelContext() }
func (runtime *Runtime) PathsExhausted() bool {
	runtime.termination.mu.Lock()
	defer runtime.termination.mu.Unlock()
	return runtime.termination.claimed && runtime.termination.cause == TerminationPathsExhausted
}
func (runtime *Runtime) HasUsableLanes() bool { return runtime.lanes.hasUsable() }
func (runtime *Runtime) DetachLane(identity sessioncontract.LaneIdentity) bool {
	return runtime.lanes.detach(identity)
}
func (runtime *Runtime) AttachedLanes() int { return runtime.lanes.len() }
func (runtime *Runtime) LaneDone(identity sessioncontract.LaneIdentity) (<-chan struct{}, bool) {
	runtime.lanes.mu.Lock()
	defer runtime.lanes.mu.Unlock()
	lane := runtime.lanes.active[identity.ID]
	if lane == nil || lane.identity != identity {
		return nil, false
	}
	return lane.done, true
}

// ConfigureLanes binds role dependencies before Start. Registry callbacks preserve
// runtime-lane then content-lane lock order and never transfer registry ownership.
func (runtime *Runtime) ConfigureLanes(queued func(sessioncontract.LaneIdentity) time.Duration, detached func(sessioncontract.LaneIdentity)) {
	runtime.lanes.mu.Lock()
	defer runtime.lanes.mu.Unlock()
	runtime.lanes.queuedContent = queued
	runtime.lanes.onDetach = detached
}

// InstallLane publishes both protocol and role ownership under the same registry
// lock. A failed role admission cannot leave a protocol-only lane visible.
func (runtime *Runtime) InstallLane(identity sessioncontract.LaneIdentity, channel protocolsession.FrameChannel, authenticator protocolsession.InboundMessageAuthenticator, requests *requestlane.Lane, admit func() error) error {
	_, err := runtime.lanes.addWithAdmission(identity, channel, authenticator, false, func(lane *RuntimeLane) error {
		if requests != nil {
			lane.requests = requests
		}
		if admit != nil {
			return admit()
		}
		return nil
	})
	return err
}
func (runtime *Runtime) NewLaneHello(grant sessioncontract.LaneAttachmentGrant) (protocolsession.LaneHello, error) {
	return protocolsession.NewLaneHello(runtime.share, runtime.sessionID, grant.LaneID, grant.LaneEpoch, grant.OperationID, grant.AttachNonce[:], runtime.keys.ReceiverToSender())
}
func (runtime *Runtime) NewLaneNonce() ([]byte, error) {
	return ReadNonzeroLaneNonce(runtime.random, protocolsession.LaneSenderNonceBytes)
}
func (runtime *Runtime) TracingEnabled() bool { return runtime.protocolOperationTracingEnabled() }
func (runtime *Runtime) ObservationContext(id protocolsession.OperationID, kind protocolsession.MessageKind) sessionobs.ProtocolObservationContext {
	return runtime.observationContext(id, kind)
}
func (runtime *Runtime) Observe(event sessionobs.ProtocolObservation) {
	runtime.protocolObservations.TryPublish(event)
}

func (outbound *Responses) SendTerminalAll(ctx context.Context, body []byte) error {
	return outbound.sendTerminalAll(ctx, body)
}

// OperationObserver is the consumer contract for a long-lived request owner.
// Binding happens before violation delivery; the adapter must only retain the
// terminal token and must not reenter the call while BindTerminalOwner runs.
type OperationObserver interface {
	BindTerminalOwner(*TerminalOwner)
	ObserveAuthenticatedOperationViolation(protocolsession.AuthenticatedOperationViolation)
}

func (client *Requests) Runtime() *Runtime {
	if client == nil {
		return nil
	}
	return client.runtime
}
func (client *Requests) Register() error { return client.register(client.runtime.router) }
func (client *Requests) Begin(ctx context.Context, kind protocolsession.MessageKind, body []byte) (*Call, error) {
	return client.begin(ctx, kind, body)
}
func (client *Requests) PrepareCall(ctx context.Context, kind protocolsession.MessageKind, body []byte) (*Call, protocolsession.Message, error) {
	return client.prepareCall(ctx, kind, body)
}
func (client *Requests) BeginCall(ctx context.Context, lane *sessioncontract.LaneIdentity, call *Call, message protocolsession.Message) error {
	return client.beginCall(ctx, lane, call, message)
}
func (client *Requests) Await(ctx context.Context, call *Call) (protocolsession.Message, error) {
	return client.await(ctx, call)
}
func (client *Requests) AwaitPeer(ctx context.Context, call *Call) (protocolsession.Message, error) {
	return client.awaitPeer(ctx, call)
}
func (client *Requests) AwaitCall(ctx context.Context, call *Call, cancelOnContext bool) (protocolsession.Message, error) {
	return client.awaitCall(ctx, call, cancelOnContext)
}
func (client *Requests) CancelAndEnd(call *Call, reason contentflow.CancelReason) error {
	return client.cancelAndEnd(call, reason)
}
func (client *Requests) End(call *Call) { client.end(call) }
func (client *Requests) SendContinuation(ctx context.Context, call *Call, kind protocolsession.MessageKind, body []byte) (protocolsession.SendOutcome, error) {
	return client.sendContinuation(ctx, call, kind, body)
}
func (call *Call) ID() protocolsession.OperationID { return call.id }
func (call *Call) ContinuationLimit() (int, bool)  { return call.continuationLimit() }
func (call *Call) OperationAuthority() (protocolsession.OperationGeneration, protocolsession.OutboundOperationPermit) {
	return call.operationAuthority()
}
func (call *Call) LastResponseLane() sessioncontract.LaneIdentity {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.lastResponseLane
}
func (call *Call) ClaimProtocolTermination() *TerminalOwner { return call.claimProtocolTermination() }
func (call *Call) RecordTraceCause(cause sessionobs.ProtocolOperationCause) {
	call.recordProtocolTraceCause(cause)
}
func (call *Call) RecordTraceFailure(err error) { call.recordProtocolTraceFailure(err) }
func (call *Call) RecordTraceCleanup(err error) { call.recordProtocolTraceCleanup(err) }
func (call *Call) Close()                       { call.close() }
func (owner *TerminalOwner) Finish(runtime *Runtime, outcome OperationOutcome) {
	owner.finish(runtime, outcome)
}

func (request LaneAttachRequest) RequestedLaneID() uint32 { return request.requestedLaneID }

func (runtime *Runtime) Close() { runtime.close() }
