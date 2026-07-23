package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestAttachedFrameChannelsRunDistinctBlocksInParallelAndFanOutTerminal(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	first, firstSenderChannel, firstReceiverChannel, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	second, secondSenderChannel, secondReceiverChannel, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	if first.ID == second.ID || first.Epoch == second.Epoch {
		t.Fatalf("attached identities = %+v and %+v", first, second)
	}
	initialID, initialEpoch := receiver.LaneIdentity()
	initial := LaneIdentity{ID: initialID, Epoch: initialEpoch}
	if !receiver.DetachLane(initial) || !sender.DetachLane(initial) {
		t.Fatal("initial lane did not detach on both peers")
	}
	if receiver.AttachedLanes() != 2 || sender.AttachedLanes() != 2 || receiver.LaneSet().Len() != 2 {
		t.Fatalf("attached lane counts = receiver %d sender %d set %d",
			receiver.AttachedLanes(), sender.AttachedLanes(), receiver.LaneSet().Len())
	}

	started := make(chan uint64, 3)
	gate := make(chan struct{})
	fixture.contentStore.blockStart = started
	fixture.contentStore.blockGate = gate
	output := make([]byte, len(fixture.fileData))
	readResult := make(chan error, 1)
	go func() {
		readResult <- receiver.BlockBroker().ReadRange(
			context.Background(), opened.LeaseID, opened.Descriptor,
			content.Range{Offset: 0, End: uint64(len(output))},
			transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
				copy(output[offset:], data)
				return nil
			}),
		)
	}()

	seen := make(map[uint64]struct{}, 2)
	for len(seen) < 2 {
		select {
		case index := <-started:
			seen[index] = struct{}{}
		case <-time.After(time.Second):
			t.Fatalf("parallel block starts = %v", seen)
		}
	}
	if firstReceiverChannel.sends.Load() < 2 || secondReceiverChannel.sends.Load() < 2 {
		t.Fatalf("lane sends before release = %d/%d; both lanes must own a request",
			firstReceiverChannel.sends.Load(), secondReceiverChannel.sends.Load())
	}
	close(gate)
	if err := <-readResult; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, fixture.fileData) {
		t.Fatal("multi-lane output differs")
	}

	if err := sender.Stop(context.Background(), "multi-lane terminal"); err != nil {
		t.Fatal(err)
	}
	if firstSenderChannel.terminals.Load() != 1 || secondSenderChannel.terminals.Load() != 1 {
		t.Fatalf("terminal fanout = %d/%d", firstSenderChannel.terminals.Load(), secondSenderChannel.terminals.Load())
	}
	if firstSenderChannel.recvCalls.Load() != 1 || firstReceiverChannel.recvCalls.Load() != 1 ||
		secondSenderChannel.recvCalls.Load() != 1 || secondReceiverChannel.recvCalls.Load() != 1 {
		t.Fatal("an attached FrameChannel acquired Recv more than once")
	}
}

func TestSuspendedInitialContentLanePreservesCatalogAndRoutesBlocksPeerFirst(t *testing.T) {
	fixture := newVerticalFixture(t)
	close(fixture.scanGate)
	initialSenderChannel, initialReceiverChannel := newObservedChannelPair()
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := fixture.senderFactory.Accept(context.Background(), initialSenderChannel)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := fixture.receiverFactory.Connect(context.Background(), initialReceiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	senderResult := <-accepted
	if senderResult.err != nil {
		receiver.Close()
		t.Fatal(senderResult.err)
	}
	sender := senderResult.runtime
	defer sender.Close()
	defer receiver.Close()

	initialID, initialEpoch := receiver.LaneIdentity()
	if initialEpoch != 0 {
		t.Fatalf("initial transcript lane epoch=%d", initialEpoch)
	}
	if _, err := receiver.LaneSet().SuspendContent(transfer.LaneIdentity{ID: initialID, Epoch: initialEpoch}); err != nil {
		t.Fatal(err)
	}
	if receiver.AttachedLanes() != 1 || receiver.LaneSet().Len() != 1 {
		t.Fatalf("suspension detached relay: attached=%d content entries=%d", receiver.AttachedLanes(), receiver.LaneSet().Len())
	}
	if _, err := receiver.Catalog().LoadDirectory(context.Background(), fixture.syntheticRoot); err != nil {
		t.Fatalf("catalog root over suspended content lane: %v", err)
	}
	if _, err := receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID); err != nil {
		t.Fatalf("catalog child over suspended content lane: %v", err)
	}
	opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}

	_, _, peerReceiverChannel, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	relaySendsBeforeContent := initialReceiverChannel.sends.Load()
	peerSendsBeforeContent := peerReceiverChannel.sends.Load()
	output := make([]byte, len(fixture.fileData))
	if err := receiver.BlockBroker().ReadRange(
		context.Background(), opened.LeaseID, opened.Descriptor,
		content.Range{Offset: 0, End: uint64(len(output))},
		transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
			copy(output[offset:], data)
			return nil
		}),
	); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, fixture.fileData) {
		t.Fatal("peer-first content changed bytes")
	}
	if got := initialReceiverChannel.sends.Load(); got != relaySendsBeforeContent {
		t.Fatalf("suspended relay carried content request: sends %d -> %d", relaySendsBeforeContent, got)
	}
	if got := peerReceiverChannel.sends.Load(); got <= peerSendsBeforeContent {
		t.Fatalf("attached peer carried no content request: sends %d -> %d", peerSendsBeforeContent, got)
	}
	if err := receiver.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
		t.Fatal(err)
	}
}

func TestLaneDetachRaceBurnsEpochAndReplayGetsSignedRejection(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	first, _, _, grant := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	var receiverDetached, senderDetached atomic.Bool
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		receiverDetached.Store(receiver.DetachLane(first))
	}()
	go func() {
		defer wait.Done()
		senderDetached.Store(sender.DetachLane(first))
	}()
	wait.Wait()
	if !receiverDetached.Load() || !senderDetached.Load() {
		t.Fatalf("detach race results = receiver %v sender %v", receiverDetached.Load(), senderDetached.Load())
	}

	_, receiverErr, senderErr := attachGrantedLane(t, fixture.senderFactory, receiver, grant)
	var receiverRejection *LaneRejectedError
	if !errors.As(receiverErr, &receiverRejection) || receiverRejection.Rejection.Code != protocolsession.LaneRejectGrantConsumed {
		t.Fatalf("receiver replay rejection = %v", receiverErr)
	}
	var senderRejection *LaneRejectedError
	if !errors.As(senderErr, &senderRejection) || senderRejection.Rejection.Code != protocolsession.LaneRejectGrantConsumed {
		t.Fatalf("sender replay rejection = %v", senderErr)
	}

	next, _, _, _ := attachObservedLane(t, fixture.senderFactory, receiver, first.ID)
	if next.ID != first.ID || next.Epoch <= first.Epoch {
		t.Fatalf("reattached identity = %+v after %+v", next, first)
	}
	if receiver.DetachLane(first) || sender.DetachLane(first) {
		t.Fatal("stale epoch detached the replacement lane")
	}
}

func TestCancelOnOneLaneStopsOperationStartedOnAnotherLane(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	first, _, firstReceiverChannel, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	second, _, secondReceiverChannel, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	initialID, initialEpoch := receiver.LaneIdentity()
	initial := LaneIdentity{ID: initialID, Epoch: initialEpoch}
	if !receiver.DetachLane(initial) || !sender.DetachLane(initial) {
		t.Fatal("initial lane did not detach")
	}

	fixture.contentStore.blockStart = make(chan uint64, 1)
	fixture.contentStore.blockGate = make(chan struct{})
	fixture.contentStore.blockStop = make(chan struct{}, 1)
	request, err := contentflow.NewBlockRequest(opened.LeaseID, []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	body, err := contentflow.EncodeBlockRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	call, err := receiver.rpc.beginOn(context.Background(), &first, protocolsession.MessageRequestBlocks, body)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.rpc.end(call)
	select {
	case <-fixture.contentStore.blockStart:
	case <-time.After(time.Second):
		t.Fatal("sender did not begin the lane-one operation")
	}
	cancelBody, _ := contentflow.EncodeCancelReason(contentflow.CancelReasonLaneRace)
	cancelMessage, _ := protocolsession.NewMessage(protocolsession.MessageCancel, &call.id, cancelBody)
	cancelLane, err := receiver.runtimeCore.lanes.selectLane(&second)
	if err != nil {
		t.Fatal(err)
	}
	_, authority := call.operationAuthority()
	receipt, err := cancelLane.writer.TryAuthorizedControl(cancelMessage, authority)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := receipt.Wait(context.Background()); err != nil || outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("cross-lane cancel = %v, %v", outcome, err)
	}
	select {
	case <-fixture.contentStore.blockStop:
	case <-time.After(time.Second):
		t.Fatal("cancel did not cross the shared operation router")
	}
	if firstReceiverChannel.sends.Load() < 2 || secondReceiverChannel.sends.Load() < 2 {
		t.Fatalf("request/cancel lane sends = %d/%d", firstReceiverChannel.sends.Load(), secondReceiverChannel.sends.Load())
	}
	select {
	case <-receiver.Done():
		t.Fatalf("idempotent cross-lane cancel terminated the session: %v", receiver.Err())
	default:
	}
}

func TestSenderMigratesSameBlockOperationAfterSendDetachRace(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver, relaySenderChannel, _ := connectObservedVerticalPair(
		t, fixture.senderFactory, fixture.receiverFactory,
	)
	defer sender.Close()
	defer receiver.Close()
	opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	peer, peerSenderChannel, _, _ := attachObservedLane(t, fixture.senderFactory, receiver, 0)
	sender.lanes.mu.Lock()
	peerSenderLane := sender.lanes.active[peer.ID]
	sender.lanes.mu.Unlock()
	if peerSenderLane == nil {
		t.Fatal("sender peer lane was not active")
	}

	fixture.contentStore.blockStart = make(chan uint64, 1)
	blockGate := make(chan struct{})
	fixture.contentStore.blockGate = blockGate
	request, _ := contentflow.NewBlockRequest(opened.LeaseID, []uint64{0})
	body, _ := contentflow.EncodeBlockRequest(request)
	call, err := receiver.rpc.beginOn(context.Background(), &peer, protocolsession.MessageRequestBlocks, body)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.contentStore.blockStart:
	case <-time.After(time.Second):
		receiver.rpc.end(call)
		t.Fatal("sender did not begin the peer-routed request")
	}
	route := sender.routes.current(call.id)
	if route == nil {
		receiver.rpc.end(call)
		t.Fatal("sender route is missing")
	}
	relaySendsBefore := relaySenderChannel.sends.Load()
	peerSendsBefore := peerSenderChannel.sends.Load()
	gate := peerSenderChannel.gateNextSendThenFail(errors.New("peer accepted frame before disconnect"))
	close(blockGate)
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		receiver.rpc.end(call)
		t.Fatal("sender did not enter the gated peer send")
	}
	close(gate.release)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fragments := 0
	completedRecord := false
	tombstonedDuplicate := false
	for {
		message, awaitErr := receiver.rpc.await(ctx, call)
		if awaitErr != nil {
			receiver.rpc.end(call)
			t.Fatal(awaitErr)
		}
		if operationID, ok := message.OperationID(); !ok || operationID != call.id {
			receiver.rpc.end(call)
			t.Fatalf("migrated response operation=%x want=%x", operationID, call.id)
		}
		switch message.Kind() {
		case protocolsession.MessageBlockFragment:
			fragments++
			assembly, assemblyErr := receiver.assembler.AcceptAuthenticated(message.Body())
			if assemblyErr != nil {
				receiver.rpc.end(call)
				t.Fatal(assemblyErr)
			}
			completedRecord = completedRecord || assembly.Status == contentflow.RecordComplete
			tombstonedDuplicate = tombstonedDuplicate || assembly.Status == contentflow.FragmentTombstoned
		case protocolsession.MessageOperationComplete:
			if fragments < 2 || !completedRecord || !tombstonedDuplicate {
				receiver.rpc.end(call)
				t.Fatalf(
					"ambiguous fragment replay was not idempotent: fragments=%d complete=%v tombstoned=%v",
					fragments, completedRecord, tombstonedDuplicate,
				)
			}
			_ = receiver.assembler.CompleteOperation(call.id)
			receiver.rpc.end(call)
			goto completed
		default:
			receiver.rpc.end(call)
			t.Fatalf("unexpected response kind %d", message.Kind())
		}
	}

completed:
	route.sendMu.Lock()
	preferredAfterSend := route.preferred
	route.sendMu.Unlock()
	if !preferredAfterSend.valid(true) {
		t.Fatal("completed send erased its synchronized route")
	}
	select {
	case <-peerSenderLane.done:
	case <-time.After(time.Second):
		t.Fatal("failed peer writer did not drain")
	}
	if fixture.contentStore.blockReads.Load() != 1 {
		t.Fatalf("same operation read source %d times", fixture.contentStore.blockReads.Load())
	}
	if relaySenderChannel.sends.Load() <= relaySendsBefore {
		t.Fatal("surviving relay carried no migrated response")
	}
	if peerSenderChannel.sends.Load() != peerSendsBefore+1 {
		t.Fatalf("failed peer attempted %d response frames", peerSenderChannel.sends.Load()-peerSendsBefore)
	}
	if sender.routes.len() != 0 || sender.operations.ActiveCount() != 0 {
		t.Fatalf("sender routes=%d active=%d", sender.routes.len(), sender.operations.ActiveCount())
	}
	receiver.rpc.mu.Lock()
	callCount := len(receiver.rpc.calls)
	receiver.rpc.mu.Unlock()
	if callCount != 0 {
		t.Fatalf("receiver retained %d RPC call(s)", callCount)
	}
}

func TestSenderResignsAmbiguousFinalForSurvivingLane(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver, relaySenderChannel, _ := connectObservedVerticalPair(
		t, fixture.senderFactory, fixture.receiverFactory,
	)
	defer sender.Close()
	defer receiver.Close()
	peer, peerSenderChannel, peerReceiverChannel, _ := attachObservedLane(
		t, fixture.senderFactory, receiver, 0,
	)
	sender.lanes.mu.Lock()
	peerSenderLane := sender.lanes.active[peer.ID]
	sender.lanes.mu.Unlock()
	if peerSenderLane == nil {
		t.Fatal("sender peer lane was not active")
	}

	emptyRanges, _ := content.NewRangeSet(nil)
	request, _ := contentflow.NewOpenRequest([]contentflow.OpenItem{{
		FileID: fixture.fileID, InitialRanges: emptyRanges,
	}})
	body, _ := contentflow.EncodeOpenRequest(request)
	relaySendsBefore := relaySenderChannel.sends.Load()
	requestSendsBefore := peerReceiverChannel.sends.Load()
	responseSendsBefore := peerSenderChannel.sends.Load()
	gate := peerSenderChannel.gateNextSendThenFail(errors.New("peer accepted final before disconnect"))
	call, err := receiver.rpc.beginOn(context.Background(), &peer, protocolsession.MessageOpenRevisions, body)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		receiver.rpc.end(call)
		t.Fatal("sender did not enter the gated final send")
	}
	route := sender.routes.current(call.id)
	if route == nil {
		receiver.rpc.end(call)
		t.Fatal("sender route is missing")
	}
	close(gate.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	message, err := receiver.rpc.await(ctx, call)
	if err != nil {
		receiver.rpc.end(call)
		t.Fatal(err)
	}
	if operationID, ok := message.OperationID(); message.Kind() != protocolsession.MessageOpenResults ||
		!ok || operationID != call.id {
		receiver.rpc.end(call)
		t.Fatalf("final response kind=%d operation=%x", message.Kind(), operationID)
	}
	receiver.rpc.end(call)
	route.sendMu.Lock()
	preferredAfterSend := route.preferred
	route.sendMu.Unlock()
	if !preferredAfterSend.valid(true) {
		t.Fatal("completed send erased its synchronized route")
	}
	select {
	case <-peerSenderLane.done:
	case <-time.After(time.Second):
		t.Fatal("ambiguous final peer writer did not drain")
	}
	if peerReceiverChannel.sends.Load() != requestSendsBefore+1 {
		t.Fatalf("receiver sent request %d times", peerReceiverChannel.sends.Load()-requestSendsBefore)
	}
	if peerSenderChannel.sends.Load() != responseSendsBefore+1 ||
		relaySenderChannel.sends.Load() <= relaySendsBefore {
		t.Fatalf(
			"final attempts peer=%d relay delta=%d",
			peerSenderChannel.sends.Load()-responseSendsBefore,
			relaySenderChannel.sends.Load()-relaySendsBefore,
		)
	}
	if sender.routes.len() != 0 || sender.operations.ActiveCount() != 0 {
		t.Fatalf("sender routes=%d active=%d", sender.routes.len(), sender.operations.ActiveCount())
	}
	select {
	case <-receiver.Done():
		t.Fatalf("cross-lane re-signature failed authentication: %v", receiver.Err())
	default:
	}
}

func TestLaneAttachBodiesRejectAlternateSemanticsAndZeroNonces(t *testing.T) {
	requestBody, err := encodeLaneAttachRequest(7)
	if err != nil {
		t.Fatal(err)
	}
	request, err := decodeLaneAttachRequest(requestBody)
	if err != nil || request.requestedLaneID != 7 {
		t.Fatalf("request = %+v, %v", request, err)
	}
	hostile, _ := protocolsession.EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion, 1: laneAttachRequestMode, 2: uint64(7), 3: uint64(0),
	})
	if _, err := decodeLaneAttachRequest(hostile); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("extra request field error = %v", err)
	}
	grant := protocolsession.LaneGrant{
		LaneID: 7, LaneEpoch: 3, OperationID: id16[protocolsession.OperationID](9),
	}
	if _, err := encodeLaneGrant(grant); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("zero grant nonce error = %v", err)
	}
	copy(grant.AttachNonce[:], bytes.Repeat([]byte{1}, protocolsession.LaneAttachNonceBytes))
	grantBody, err := encodeLaneGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeLaneGrant(grantBody, grant.OperationID)
	if err != nil || decoded.LaneID != grant.LaneID || decoded.LaneEpoch != grant.LaneEpoch || decoded.AttachNonce != grant.AttachNonce {
		t.Fatalf("grant = %+v, %v", decoded, err)
	}
	readerBytes := append(make([]byte, protocolsession.LaneAttachNonceBytes), bytes.Repeat([]byte{2}, protocolsession.LaneAttachNonceBytes)...)
	nonce, err := readNonzeroLaneNonce(bytes.NewReader(readerBytes), protocolsession.LaneAttachNonceBytes)
	if err != nil || !bytes.Equal(nonce, bytes.Repeat([]byte{2}, protocolsession.LaneAttachNonceBytes)) {
		t.Fatalf("retried nonce = %x, %v", nonce, err)
	}
	if _, err := readNonzeroLaneNonce(bytes.NewReader(make([]byte, laneNonceAttempts)), 1); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("zero nonce source error = %v", err)
	}
}

func TestSenderStopRejectsAfterCoreClosure(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	laneID, laneEpoch := receiver.LaneIdentity()
	identity := LaneIdentity{ID: laneID, Epoch: laneEpoch}
	if !receiver.DetachLane(identity) || !sender.DetachLane(identity) {
		t.Fatal("initial lane did not detach")
	}
	first := sender.Stop(context.Background(), "no lane")
	second := sender.Stop(context.Background(), "different caller")
	if !errors.Is(first, ErrRuntimeClosed) || !errors.Is(second, ErrRuntimeClosed) {
		t.Fatalf("post-close stop errors = %v / %v", first, second)
	}
}

func TestAdmitChannelSeparatesLaneRecoveryFromProtocolSessionReplacement(t *testing.T) {
	fixture := newVerticalFixture(t)
	initialSenderChannel, initialReceiverChannel := newMemoryChannelPair()
	initialAdmission := make(chan struct {
		value SenderChannelAdmission
		err   error
	}, 1)
	go func() {
		value, err := fixture.senderFactory.AdmitChannel(context.Background(), initialSenderChannel)
		initialAdmission <- struct {
			value SenderChannelAdmission
			err   error
		}{value: value, err: err}
	}()
	receiver, err := fixture.receiverFactory.Connect(context.Background(), initialReceiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	first := <-initialAdmission
	if first.err != nil || first.value.Kind != SenderChannelNewProtocolSession || first.value.Session == nil {
		t.Fatalf("initial admission = %+v, %v", first.value, first.err)
	}
	sender := first.value.Session
	oldSessionID := receiver.ProtocolSessionID()
	oldInitialLaneID, oldInitialLaneEpoch := receiver.LaneIdentity()
	if sender.ProtocolSessionID() != oldSessionID {
		t.Fatal("fresh transcript derived different session identities")
	}
	t.Cleanup(receiver.Close)
	t.Cleanup(sender.Close)

	grant, err := receiver.RequestLane(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	attachedSenderChannel, attachedReceiverChannel := newMemoryChannelPair()
	attachedAdmission := make(chan struct {
		value SenderChannelAdmission
		err   error
	}, 1)
	go func() {
		value, err := fixture.senderFactory.AdmitChannel(context.Background(), attachedSenderChannel)
		attachedAdmission <- struct {
			value SenderChannelAdmission
			err   error
		}{value: value, err: err}
	}()
	receiverLane, err := receiver.AttachLane(context.Background(), grant, attachedReceiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	attached := <-attachedAdmission
	if attached.err != nil || attached.value.Kind != SenderChannelAttachedLane || attached.value.Session != nil ||
		attached.value.Lane != receiverLane {
		t.Fatalf("lane admission = %+v, %v", attached.value, attached.err)
	}

	mismatchedGrant, err := receiver.RequestLane(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	wrongShare := fixture.share
	wrongShare[0] ^= 0xff
	mismatchedHello, err := protocolsession.NewLaneHello(
		wrongShare, oldSessionID, mismatchedGrant.LaneID, mismatchedGrant.LaneEpoch,
		mismatchedGrant.OperationID, mismatchedGrant.AttachNonce[:], receiver.keys.ReceiverToSender(),
	)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedSender, mismatchedPeer := newMemoryChannelPair()
	if err := mismatchedPeer.Send(context.Background(), framechannel.Frame(mismatchedHello.Encoded())); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.senderFactory.AdmitChannel(context.Background(), mismatchedSender); !errors.Is(err, ErrHandshake) {
		t.Fatalf("ShareInstance-mismatched lane error = %v", err)
	}
	if mismatchedSender.State() != framechannel.Closed {
		t.Fatal("ShareInstance-mismatched lane remained open")
	}
	_ = mismatchedPeer.Close()

	staleGrant, err := receiver.RequestLane(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	staleHello := laneHelloForGrant(t, receiver, staleGrant)
	_ = initialReceiverChannel.Close()
	_ = initialSenderChannel.Close()
	waitSessionCondition(t, "surviving attached lane", func() bool {
		return receiver.AttachedLanes() == 1 && sender.AttachedLanes() == 1
	})
	select {
	case <-receiver.Done():
		t.Fatal("relay-lane loss ended a session with a surviving lane")
	default:
	}

	_ = attachedReceiverChannel.Close()
	_ = attachedSenderChannel.Close()
	for name, done := range map[string]<-chan struct{}{"receiver": receiver.Done(), "sender": sender.Done()} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s session survived all-lane loss", name)
		}
	}
	waitSessionCondition(t, "sender retires old ProtocolSession", func() bool {
		return fixture.senderFactory.ActiveSessions() == 0
	})

	staleSender, stalePeer := newMemoryChannelPair()
	if err := stalePeer.Send(context.Background(), framechannel.Frame(staleHello)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.senderFactory.AdmitChannel(context.Background(), staleSender); !errors.Is(err, ErrHandshake) {
		t.Fatalf("retired-session lane error = %v", err)
	}
	if staleSender.State() != framechannel.Closed {
		t.Fatal("retired-session lane remained open")
	}
	_ = stalePeer.Close()

	freshSenderChannel, freshReceiverChannel := newMemoryChannelPair()
	freshAdmission := make(chan struct {
		value SenderChannelAdmission
		err   error
	}, 1)
	go func() {
		value, err := fixture.senderFactory.AdmitChannel(context.Background(), freshSenderChannel)
		freshAdmission <- struct {
			value SenderChannelAdmission
			err   error
		}{value: value, err: err}
	}()
	freshReceiver, err := fixture.receiverFactory.Connect(context.Background(), freshReceiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	fresh := <-freshAdmission
	if fresh.err != nil || fresh.value.Kind != SenderChannelNewProtocolSession || fresh.value.Session == nil {
		t.Fatalf("replacement admission = %+v, %v", fresh.value, fresh.err)
	}
	if freshReceiver.ProtocolSessionID() == oldSessionID || fresh.value.Session.ProtocolSessionID() != freshReceiver.ProtocolSessionID() {
		t.Fatal("all-lane recovery reused the retired ProtocolSession identity")
	}
	freshInitialLaneID, freshInitialLaneEpoch := freshReceiver.LaneIdentity()
	if oldInitialLaneEpoch != 0 || freshInitialLaneEpoch != 0 || freshInitialLaneID == oldInitialLaneID {
		t.Fatalf("replacement initial lanes = old %d/%d fresh %d/%d",
			oldInitialLaneID, oldInitialLaneEpoch, freshInitialLaneID, freshInitialLaneEpoch)
	}
	t.Cleanup(freshReceiver.Close)
	t.Cleanup(fresh.value.Session.Close)

	if err := fixture.senderFactory.Stop(context.Background(), "explicit stop"); err != nil {
		t.Fatal(err)
	}
	stoppedCandidate, stoppedPeer := newMemoryChannelPair()
	if _, err := fixture.senderFactory.AdmitChannel(context.Background(), stoppedCandidate); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("post-stop recovery admission error = %v", err)
	}
	_ = stoppedCandidate.Close()
	_ = stoppedPeer.Close()
}

func waitSessionCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestForgedLaneProofIsSilentAndDoesNotConsumeGrant(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	grant, err := receiver.RequestLane(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := protocolsession.NewLaneHello(
		fixture.share, receiver.ProtocolSessionID(), grant.LaneID, grant.LaneEpoch,
		grant.OperationID, grant.AttachNonce[:], receiver.keys.ReceiverToSender(),
	)
	if err != nil {
		t.Fatal(err)
	}
	forged := hello.Encoded()
	forged[len(forged)-1] ^= 1
	senderChannel, receiverChannel := newObservedChannelPair()
	result := make(chan error, 1)
	go func() {
		_, attachErr := fixture.senderFactory.Attach(context.Background(), senderChannel)
		result <- attachErr
	}()
	response := receiverChannel.Recv()
	if err := receiverChannel.Send(context.Background(), framechannel.Frame(forged)); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrHandshake) {
		t.Fatalf("forged attach error = %v", err)
	}
	select {
	case frame := <-response:
		t.Fatalf("forged proof received reflected frame %x", frame)
	default:
	}
	if senderChannel.sends.Load() != 0 {
		t.Fatalf("forged proof reflected %d signed responses", senderChannel.sends.Load())
	}
	identity, receiverErr, senderErr := attachGrantedLane(t, fixture.senderFactory, receiver, grant)
	if receiverErr != nil || senderErr != nil || identity.ID != grant.LaneID || identity.Epoch != grant.LaneEpoch {
		t.Fatalf("valid retry after forged proof = %+v, %v/%v", identity, receiverErr, senderErr)
	}
}

func TestAdmitPeerChannelCannotRouteAProofIntoSiblingProtocolSession(t *testing.T) {
	fixture := newVerticalFixture(t)
	firstSender, firstReceiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	secondSender, secondReceiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer firstSender.Close()
	defer firstReceiver.Close()
	defer secondSender.Close()
	defer secondReceiver.Close()
	grant, err := firstReceiver.RequestLane(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	wrongSenderChannel, wrongReceiverChannel := newMemoryChannelPair()
	wrongResult := make(chan error, 1)
	wrongContext, cancelWrong := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelWrong()
	go func() {
		_, admitErr := secondSender.AdmitPeerChannel(wrongContext, wrongSenderChannel)
		wrongResult <- admitErr
	}()
	if _, err := firstReceiver.AttachLane(wrongContext, grant, wrongReceiverChannel); err == nil {
		t.Fatal("receiver attached a peer lane through a sibling ProtocolSession")
	}
	if err := <-wrongResult; !errors.Is(err, ErrHandshake) {
		t.Fatalf("sibling peer admission error = %v", err)
	}

	rightSenderChannel, rightReceiverChannel := newMemoryChannelPair()
	rightResult := make(chan struct {
		identity LaneIdentity
		err      error
	}, 1)
	go func() {
		identity, admitErr := firstSender.AdmitPeerChannel(context.Background(), rightSenderChannel)
		rightResult <- struct {
			identity LaneIdentity
			err      error
		}{identity: identity, err: admitErr}
	}()
	receiverIdentity, err := firstReceiver.AttachLane(context.Background(), grant, rightReceiverChannel)
	if err != nil {
		t.Fatalf("attach exact-session peer lane after sibling rejection: %v", err)
	}
	right := <-rightResult
	if right.err != nil || right.identity != receiverIdentity {
		t.Fatalf("exact peer admission = %#v, %v; receiver=%#v", right.identity, right.err, receiverIdentity)
	}
	senderRelay, err := firstSender.lanes.selectLane(&firstSender.initial)
	if err != nil {
		t.Fatal(err)
	}
	receiverRelay, err := firstReceiver.lanes.selectLane(&firstReceiver.initial)
	if err != nil {
		t.Fatal(err)
	}
	_ = senderRelay.channel.Close()
	_ = receiverRelay.channel.Close()
	waitSessionCondition(t, "exact-session peer survives relay loss", func() bool {
		return firstSender.AttachedLanes() == 1 && firstReceiver.AttachedLanes() == 1
	})
	select {
	case <-firstReceiver.Done():
		t.Fatal("relay loss ended the receiver despite its admitted peer lane")
	default:
	}
}

type observedMemoryChannel struct {
	*memoryChannel
	sends     atomic.Int32
	terminals atomic.Int32
	gateMu    sync.Mutex
	nextGate  *observedSendGate
}

type observedSendGate struct {
	started        chan struct{}
	release        chan struct{}
	errorAfterSend error
}

func (channel *observedMemoryChannel) gateNextSend() *observedSendGate {
	gate := &observedSendGate{started: make(chan struct{}), release: make(chan struct{})}
	channel.gateMu.Lock()
	channel.nextGate = gate
	channel.gateMu.Unlock()
	return gate
}

func (channel *observedMemoryChannel) gateNextSendThenFail(err error) *observedSendGate {
	gate := channel.gateNextSend()
	gate.errorAfterSend = err
	return gate
}

func (channel *observedMemoryChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.sends.Add(1)
	channel.gateMu.Lock()
	gate := channel.nextGate
	channel.nextGate = nil
	channel.gateMu.Unlock()
	if gate != nil {
		close(gate.started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate.release:
		}
	}
	err := channel.memoryChannel.Send(ctx, frame)
	if err == nil && gate != nil && gate.errorAfterSend != nil {
		return gate.errorAfterSend
	}
	return err
}

func (channel *observedMemoryChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	channel.terminals.Add(1)
	return channel.memoryChannel.Send(ctx, frame)
}

func newObservedChannelPair() (*observedMemoryChannel, *observedMemoryChannel) {
	sender, receiver := newMemoryChannelPair()
	return &observedMemoryChannel{memoryChannel: sender}, &observedMemoryChannel{memoryChannel: receiver}
}

func connectObservedVerticalPair(
	t *testing.T,
	senderFactory *SenderFactory,
	receiverFactory *ReceiverFactory,
) (*SenderRuntime, *ReceiverRuntime, *observedMemoryChannel, *observedMemoryChannel) {
	t.Helper()
	senderChannel, receiverChannel := newObservedChannelPair()
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := receiverFactory.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	result := <-accepted
	if result.err != nil {
		receiver.Close()
		t.Fatal(result.err)
	}
	return result.runtime, receiver, senderChannel, receiverChannel
}

func attachObservedLane(
	t *testing.T,
	factory *SenderFactory,
	receiver *ReceiverRuntime,
	requestedLaneID uint32,
) (LaneIdentity, *observedMemoryChannel, *observedMemoryChannel, LaneAttachmentGrant) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	grant, err := receiver.RequestLane(ctx, requestedLaneID)
	if err != nil {
		t.Fatalf("request lane: %v", err)
	}
	senderChannel, receiverChannel := newObservedChannelPair()
	result, receiverErr, senderErr := attachGrantedLaneWithChannels(
		ctx, factory, receiver, grant, senderChannel, receiverChannel,
	)
	if receiverErr != nil || senderErr != nil {
		t.Fatalf("attach lane: receiver=%v sender=%v", receiverErr, senderErr)
	}
	return result, senderChannel, receiverChannel, grant
}

func attachGrantedLane(
	t *testing.T,
	factory *SenderFactory,
	receiver *ReceiverRuntime,
	grant LaneAttachmentGrant,
) (LaneIdentity, error, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	senderChannel, receiverChannel := newObservedChannelPair()
	return attachGrantedLaneWithChannels(ctx, factory, receiver, grant, senderChannel, receiverChannel)
}

func attachGrantedLaneWithChannels(
	ctx context.Context,
	factory *SenderFactory,
	receiver *ReceiverRuntime,
	grant LaneAttachmentGrant,
	senderChannel *observedMemoryChannel,
	receiverChannel *observedMemoryChannel,
) (LaneIdentity, error, error) {
	senderResult := make(chan struct {
		identity LaneIdentity
		err      error
	}, 1)
	go func() {
		identity, err := factory.Attach(ctx, senderChannel)
		senderResult <- struct {
			identity LaneIdentity
			err      error
		}{identity: identity, err: err}
	}()
	receiverIdentity, receiverErr := receiver.AttachLane(ctx, grant, receiverChannel)
	senderAttached := <-senderResult
	if receiverErr == nil && senderAttached.err == nil && receiverIdentity != senderAttached.identity {
		return LaneIdentity{}, errors.New("lane identities differ"), nil
	}
	return receiverIdentity, receiverErr, senderAttached.err
}
