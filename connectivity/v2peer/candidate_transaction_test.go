package v2peer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

const authenticatedCandidateReplays = protocolsession.RouterControlQueueLimit * 2

var errCandidateDeliveredBeforeLaneFailure = errors.New("candidate frame delivered before lane failure")

type candidateTransactionPipe struct {
	mu     sync.Mutex
	inbox  [2]chan framechannel.Frame
	closed [2]bool
}

type candidateTransactionChannel struct {
	pipe  *candidateTransactionPipe
	index int

	sends             atomic.Int64
	deliveredFailures atomic.Int64
	failureMu         sync.Mutex
	nextDeliveredErr  error
}

func newCandidateTransactionChannelPair() (*candidateTransactionChannel, *candidateTransactionChannel) {
	pipe := &candidateTransactionPipe{inbox: [2]chan framechannel.Frame{
		make(chan framechannel.Frame, 2_048), make(chan framechannel.Frame, 2_048),
	}}
	return &candidateTransactionChannel{pipe: pipe}, &candidateTransactionChannel{pipe: pipe, index: 1}
}

func (channel *candidateTransactionChannel) armNextDeliveredSendError(err error) {
	channel.failureMu.Lock()
	channel.nextDeliveredErr = err
	channel.failureMu.Unlock()
}

func (channel *candidateTransactionChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.pipe.mu.Lock()
	defer channel.pipe.mu.Unlock()
	target := 1 - channel.index
	if channel.pipe.closed[channel.index] || channel.pipe.closed[target] {
		return io.ErrClosedPipe
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	channel.pipe.inbox[target] <- bytes.Clone(frame)
	channel.sends.Add(1)

	channel.failureMu.Lock()
	err := channel.nextDeliveredErr
	channel.nextDeliveredErr = nil
	channel.failureMu.Unlock()
	if err != nil {
		// The frame is deliberately published before the error. This models the
		// transport ambiguity that requires outbound replay authority across lanes.
		channel.deliveredFailures.Add(1)
	}
	return err
}

func (channel *candidateTransactionChannel) SendTerminal(
	ctx context.Context,
	frame framechannel.Frame,
) error {
	return channel.Send(ctx, frame)
}

func (channel *candidateTransactionChannel) Recv() <-chan framechannel.Frame {
	return channel.pipe.inbox[channel.index]
}

func (channel *candidateTransactionChannel) State() framechannel.ChannelState {
	channel.pipe.mu.Lock()
	defer channel.pipe.mu.Unlock()
	if channel.pipe.closed[channel.index] {
		return framechannel.Closed
	}
	return framechannel.Open
}

func (channel *candidateTransactionChannel) Close() error {
	channel.pipe.mu.Lock()
	if !channel.pipe.closed[channel.index] {
		channel.pipe.closed[channel.index] = true
		close(channel.pipe.inbox[channel.index])
	}
	channel.pipe.mu.Unlock()
	return nil
}

type candidateRuntimeHarness struct {
	ctx               context.Context
	factory           *sessionruntime.SenderFactory
	receiver          *sessionruntime.ReceiverRuntime
	sender            *sessionruntime.SenderRuntime
	senderPeer        *testPeerConnection
	initialSenderLane *candidateTransactionChannel
}

func newCandidateRuntimeHarness(t *testing.T, maxCandidates int) *candidateRuntimeHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	selected := filepath.Join(t.TempDir(), "candidate-transaction.txt")
	if err := os.WriteFile(selected, []byte("candidate transaction"), 0o600); err != nil {
		t.Fatal(err)
	}
	preparedSender, err := liveshare.PrepareSender(ctx, liveshare.SenderConfig{
		Paths: []string{selected}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := preparedSender.Close(); err != nil {
			t.Errorf("close prepared sender: %v", err)
		}
	})
	preparedReceiver, err := liveshare.PrepareReceiver(liveshare.ReceiverConfig{
		Capability:       preparedSender.Capability(),
		DescriptorObject: preparedSender.Registration().Descriptor,
		PeerControls: v2signal.ReceiverControlValidator{
			MaximumCandidates: maxCandidates,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(preparedReceiver.Close)

	senderPeer := newTestPeerConnection()
	senderPeerFactory := mustTestFactory(t, Config{
		MaxCandidates: maxCandidates,
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return senderPeer, nil
		}),
	})
	runtimeFactory, err := preparedSender.NewRuntimeFactory(liveshare.RuntimeFactoryConfig{
		TerminalConnectivity: sessionruntime.TerminalConnectivityFuncs{
			CleanupFunc: func(context.Context) error { return nil },
		},
		PeerHandlers: senderPeerFactory,
	})
	if err != nil {
		t.Fatal(err)
	}

	initialSenderLane, initialReceiverLane := newCandidateTransactionChannelPair()
	type acceptedRuntime struct {
		runtime *sessionruntime.SenderRuntime
		err     error
	}
	accepted := make(chan acceptedRuntime, 1)
	go func() {
		runtime, acceptErr := runtimeFactory.Accept(ctx, initialSenderLane)
		accepted <- acceptedRuntime{runtime: runtime, err: acceptErr}
	}()
	receiverRuntime, err := preparedReceiver.Connect(ctx, initialReceiverLane)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiverRuntime.Close)
	acceptedResult := receiveTest(t, accepted)
	if acceptedResult.err != nil {
		t.Fatal(acceptedResult.err)
	}
	t.Cleanup(acceptedResult.runtime.Close)
	return &candidateRuntimeHarness{
		ctx: ctx, factory: runtimeFactory, receiver: receiverRuntime, sender: acceptedResult.runtime,
		senderPeer: senderPeer, initialSenderLane: initialSenderLane,
	}
}

func TestSenderCandidateDeliveredUnknownMigratesThroughAuthenticatedReceiverOnce(t *testing.T) {
	harness := newCandidateRuntimeHarness(t, 2)
	ctx, runtimeFactory, receiverRuntime := harness.ctx, harness.factory, harness.receiver
	senderPeer, initialSenderLane := harness.senderPeer, harness.initialSenderLane

	receiverPeer := newReceiverTestPeerConnection()
	receiverDataChannel := newReceiverTestChannel()
	receiverPeerFactory, err := NewReceiverFactory(ReceiverFactoryConfig{
		MaxCandidates:  2,
		AttemptTimeout: 5 * time.Second,
		Random:         bytes.NewReader(bytes.Repeat([]byte{0x71}, 256)),
		PeerConnections: ReceiverPeerConnectionFactoryFunc(func(pion.Configuration) (ReceiverPeerConnection, error) {
			return receiverPeer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			return receiverDataChannel, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	receiverSignaling, err := NewRuntimeReceiverSignaling(receiverRuntime)
	if err != nil {
		t.Fatal(err)
	}
	receiverAttempt, err := receiverPeerFactory.Start(ctx, receiverSignaling, receiverRuntime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := receiverAttempt.Close(); err != nil {
			t.Errorf("close receiver peer attempt: %v", err)
		}
	})
	answer := receiveTest(t, receiverPeer.remote)
	if answer.Type != pion.SDPTypeAnswer {
		t.Fatalf("receiver remote description type=%s", answer.Type)
	}

	grant, err := receiverRuntime.RequestLane(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	replacementSenderLane, replacementReceiverLane := newCandidateTransactionChannelPair()
	attached := make(chan struct {
		identity sessionruntime.LaneIdentity
		err      error
	}, 1)
	go func() {
		identity, attachErr := runtimeFactory.Attach(ctx, replacementSenderLane)
		attached <- struct {
			identity sessionruntime.LaneIdentity
			err      error
		}{identity: identity, err: attachErr}
	}()
	receiverIdentity, err := receiverRuntime.AttachLane(ctx, grant, replacementReceiverLane)
	if err != nil {
		t.Fatal(err)
	}
	senderAttachment := receiveTest(t, attached)
	if senderAttachment.err != nil || senderAttachment.identity != receiverIdentity {
		t.Fatalf(
			"replacement lane sender=%+v err=%v receiver=%+v",
			senderAttachment.identity,
			senderAttachment.err,
			receiverIdentity,
		)
	}

	replacementBaseline := replacementSenderLane.sends.Load()
	initialSenderLane.armNextDeliveredSendError(errCandidateDeliveredBeforeLaneFailure)
	localCandidate := &pion.ICECandidate{
		Foundation: "1", Priority: 1, Address: "127.0.0.1", Protocol: pion.ICEProtocolUDP,
		Port: 43210, Typ: pion.ICECandidateTypeHost, Component: 1,
	}
	wantCandidate := localCandidate.ToJSON().Candidate
	senderPeer.emitCandidate(localCandidate)
	if added := receiveTest(t, receiverPeer.added); added.Candidate != wantCandidate {
		t.Fatalf("receiver Pion candidate=%#v, want %q", added, wantCandidate)
	}
	waitForTest(t, func() bool {
		return initialSenderLane.deliveredFailures.Load() == 1 &&
			replacementSenderLane.sends.Load() == replacementBaseline+1
	})
	barrierCandidate := &pion.ICECandidate{
		Foundation: "2", Priority: 2, Address: "127.0.0.2", Protocol: pion.ICEProtocolUDP,
		Port: 43211, Typ: pion.ICECandidateTypeHost, Component: 1,
	}
	wantBarrier := barrierCandidate.ToJSON().Candidate
	senderPeer.emitCandidate(barrierCandidate)
	if added := receiveTest(t, receiverPeer.added); added.Candidate != wantBarrier {
		t.Fatalf("receiver Pion barrier candidate=%#v, want %q", added, wantBarrier)
	}
	waitForTest(t, func() bool {
		return replacementSenderLane.sends.Load() == replacementBaseline+2
	})

	select {
	case duplicate := <-receiverPeer.added:
		t.Fatalf("authenticated cross-lane replay reached Pion twice: %#v", duplicate)
	default:
	}
	select {
	case <-receiverAttempt.Done():
		t.Fatalf("authenticated exact replay consumed receiver candidate quota: %v", receiverAttempt.Err())
	default:
	}
	if senderErr, receiverErr := harness.sender.Err(), receiverRuntime.Err(); senderErr != nil || receiverErr != nil {
		t.Fatalf("candidate migration damaged runtime health sender=%v receiver=%v", senderErr, receiverErr)
	}
}

func TestReceiverCandidateConcurrentAuthenticatedIngressChargesSenderOnce(t *testing.T) {
	harness := newCandidateRuntimeHarness(t, 2)
	binding := testBinding(141)
	offerBody, err := v2signal.EncodeOffer(v2signal.Offer{
		Binding: binding,
		SDP:     "v=0\r\ns=authenticated-receiver-candidate\r\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := harness.receiver.OpenPeerOperation(harness.ctx, offerBody)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		termination := operation.Terminate(harness.ctx)
		if diagnostics := termination.Diagnostics().Components(); len(diagnostics) != 0 {
			t.Errorf("terminate receiver peer operation diagnostics: %+v", diagnostics)
		}
	})
	received := operation.Receive(harness.ctx)
	if termination, ok := received.Termination(); ok {
		t.Fatalf("receiver peer operation terminated before answer: %+v", termination)
	}
	control, ok := received.Control()
	if !ok {
		t.Fatal("receiver peer operation returned neither answer nor termination")
	}
	answer, err := v2signal.DecodeAnswer(control.Body())
	if err != nil || control.Kind() != protocolsession.MessagePeerAnswer || answer.Binding != binding {
		t.Fatalf("authenticated answer kind=%v answer=%#v err=%v", control.Kind(), answer, err)
	}

	candidate, candidateBody := testCandidate(t, binding, 142)
	type candidateSendResult struct {
		disposition protocolsession.OperationDisposition
		err         error
	}
	resultsByReplay := make(chan candidateSendResult, authenticatedCandidateReplays)
	var replays sync.WaitGroup
	for replay := 0; replay < authenticatedCandidateReplays; replay++ {
		replays.Add(1)
		go func() {
			defer replays.Done()
			disposition, sendErr := operation.SendCandidate(harness.ctx, candidateBody)
			resultsByReplay <- candidateSendResult{disposition: disposition, err: sendErr}
		}()
	}
	replays.Wait()
	close(resultsByReplay)
	delivered, dropped := 0, 0
	for result := range resultsByReplay {
		if result.err != nil {
			t.Fatalf("authenticated receiver candidate replay: %v", result.err)
		}
		switch result.disposition {
		case protocolsession.OperationDeliver:
			delivered++
		case protocolsession.OperationDrop:
			dropped++
		default:
			t.Fatalf("authenticated receiver candidate disposition=%d", result.disposition)
		}
	}
	if delivered != 1 || dropped != authenticatedCandidateReplays-1 {
		t.Fatalf("authenticated receiver candidate dispositions delivered=%d dropped=%d", delivered, dropped)
	}
	barrier, barrierBody := testCandidate(t, binding, 143)
	barrierDisposition, err := operation.SendCandidate(harness.ctx, barrierBody)
	if err != nil || barrierDisposition != protocolsession.OperationDeliver {
		t.Fatalf("authenticated receiver barrier candidate disposition=%d err=%v", barrierDisposition, err)
	}
	for index, want := range []string{candidate.Candidate, barrier.Candidate} {
		if added := receiveTest(t, harness.senderPeer.added); added.Candidate != want {
			t.Fatalf("sender Pion candidate %d=%#v, want %q", index, added, want)
		}
	}
	select {
	case duplicate := <-harness.senderPeer.added:
		t.Fatalf("authenticated receiver replay reached sender Pion twice: %#v", duplicate)
	default:
	}
	select {
	case <-harness.senderPeer.closed:
		t.Fatal("authenticated exact replay consumed sender candidate quota")
	default:
	}
	if senderErr, receiverErr := harness.sender.Err(), harness.receiver.Err(); senderErr != nil || receiverErr != nil {
		t.Fatalf("authenticated ingress damaged runtime health sender=%v receiver=%v", senderErr, receiverErr)
	}
}
