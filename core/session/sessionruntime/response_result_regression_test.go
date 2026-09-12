package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestResponsePreparationFailureOwnsNoAttempt(t *testing.T) {
	for _, mode := range []string{"authority", "signing", "encoding"} {
		t.Run(mode, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			recorder := newProtocolTraceRecorder(runtime)
			id := id16[protocolsession.OperationID](140)
			request, _ := protocolsession.NewMessage(protocolsession.MessageReleaseLease, &id, []byte{0xa0})
			ctx, _ := testOutboundOperationContext(t, runtime, runtime.initial, request)
			outbound := senderOutbound{runtime: runtime}
			if mode == "authority" {
				ctx = context.Background()
			}
			if mode == "encoding" {
				err := outbound.SendOperationError(ctx, id, contentflow.OperationFailure{Scope: 255})
				if err == nil {
					t.Fatal("invalid failure encoded")
				}
			} else {
				body, _ := contentflow.EncodeOperationComplete(0)
				result, err := outbound.SendControl(ctx, protocolsession.MessageOperationComplete, id, body)
				if err == nil || result.Started() || result.Evidence() != protocolsession.ResponseSendEvidenceDefinitelyNotSent || result.AttemptCount() != 0 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			}
			facts := recorder.facts()
			if len(facts) != 1 {
				t.Fatalf("facts=%v", facts)
			}
			fact, ok := facts[0].(ResponseSendNotStarted)
			if !ok || fact.ResponseSequence() == 0 || fact.Result().Started() || fact.ObservedAt().IsZero() {
				t.Fatalf("fact=%v", facts[0])
			}
		})
	}
}

func TestResponseCleanupFailurePreservesConfirmedEvidence(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := newProtocolTraceRecorder(runtime)
	lane, _ := runtime.lanes.selectLane(&runtime.initial)
	writerCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lane.writer.Run(writerCtx) }()
	defer func() { stop(); <-done }()
	id := id16[protocolsession.OperationID](141)
	request, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &id, []byte{0xa0})
	ctx, _ := testOutboundOperationContext(t, runtime, runtime.initial, request)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fragments, _ := contentflow.FragmentRecord(id, []byte("record"))
	outbound := senderOutbound{runtime: runtime}
	result, err := outbound.executeResponse(ctx, fragments[0].Kind(), id, ProtocolErrorContent{}, func(transaction *outboundTransaction) (outboundLaneAttempt, error) {
		return func(lane selectedLane, _ protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
			receipt, err := lane.writer.TryAuthorizedData(fragments[0], transaction.authority)
			if err != nil {
				return receipt, err
			}
			completion := receipt.Await(context.Background())
			if completion.Outcome != protocolsession.SendOutcomeTransportConfirmed {
				t.Errorf("completion=%+v", completion)
			}
			runtime.routes.releaseRoute(id, transaction.route)
			cancel()
			return receipt, nil
		}, nil
	})
	if !errors.Is(err, ErrOperationMissing) || result.Evidence() != protocolsession.ResponseSendEvidenceTransportConfirmed ||
		result.Cleanup() != protocolsession.SendCleanupFailed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	facts := recorder.facts()
	if len(facts) != 1 || facts[0].(ResponseSendReturned).Result() != result {
		t.Fatalf("facts=%v", facts)
	}
}

func TestPendingResponseAndLateSettlementRemainIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		recorder := newProtocolTraceRecorder(runtime)
		runtime.startReceiptObservations()
		base, peer := newMemoryChannelPair()
		defer peer.Close()
		channel := &blockingRuntimeLaneChannel{memoryChannel: base, started: make(chan struct{}), release: make(chan struct{})}
		identity := LaneIdentity{ID: 2, Epoch: 7}
		lane, err := runtime.lanes.add(identity, channel, permissiveInboundAuthenticator(), false)
		if err != nil {
			t.Fatal(err)
		}
		writerCtx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lane.writer.Run(writerCtx) }()
		id := id16[protocolsession.OperationID](142)
		request, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &id, []byte{0xa0})
		ctx, _ := testOutboundOperationContext(t, runtime, identity, request)
		ctx, cancel := context.WithCancel(ctx)
		fragments, _ := contentflow.FragmentRecord(id, []byte("record"))
		outbound := senderOutbound{runtime: runtime}
		sent := make(chan error, 1)
		go func() { sent <- outbound.SendFragment(ctx, fragments[0]) }()
		<-channel.started
		cancel()
		if err := <-sent; !errors.Is(err, context.Canceled) {
			t.Fatalf("send=%v", err)
		}
		facts := recorder.facts()
		if len(facts) != 1 {
			t.Fatalf("return facts=%v", facts)
		}
		returned := facts[0].(ResponseSendReturned)
		before := returned.Result()
		pending, ok := before.PendingAttempt()
		if !ok || pending.LaneID != identity.ID || pending.LaneEpoch != identity.Epoch ||
			before.Evidence() != protocolsession.ResponseSendEvidenceUncertain {
			t.Fatalf("pending=%+v result=%+v", pending, before)
		}
		time.Sleep(time.Second)
		close(channel.release)
		synctest.Wait()
		facts = recorder.facts()
		if len(facts) != 2 {
			t.Fatalf("settlement facts=%v", facts)
		}
		settled := facts[1].(SendAttemptSettled)
		if settled.Attempt().Identity() != pending || settled.Attempt().Outcome() != protocolsession.SendOutcomeTransportConfirmed ||
			!settled.Attempt().Settled() || !settled.ObservedAt().After(returned.ObservedAt()) ||
			returned.Result() != before {
			t.Fatalf("return=%+v settled=%+v", returned, settled)
		}
		stop()
		<-done
		runtime.abortBeforeStart()
		completion := recorder.producer.Complete()
		if completion.Enqueued != 2 || completion.CapacityDropped != 0 {
			t.Fatalf("completion=%+v", completion)
		}
	})
}

func TestInvalidLaterReceiptCannotProveWholeResponseNotSent(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	secondBase, peer := newMemoryChannelPair()
	defer peer.Close()
	if _, err := runtime.lanes.add(LaneIdentity{ID: 2, Epoch: 1}, secondBase, permissiveInboundAuthenticator(), false); err != nil {
		t.Fatal(err)
	}
	id := id16[protocolsession.OperationID](143)
	request, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &id, []byte{0xa0})
	ctx, _ := testOutboundOperationContext(t, runtime, runtime.initial, request)
	transaction, err := beginOutboundTransaction(runtime, ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	result, err := transaction.Run(ctx, func(selectedLane, protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
		count++
		if count == 1 {
			return protocolsession.SendReceipt{}, protocolsession.ErrControlQueueFull
		}
		return protocolsession.SendReceipt{}, nil
	})
	transaction.Close()
	if !errors.Is(err, errOutboundReplayAuthority) || result.Evidence() != protocolsession.ResponseSendEvidenceUninitialized ||
		result.AttemptCount() != 1 || result.End() != protocolsession.ResponseSendEndInvalidReceipt {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	previous, _ := result.Attempt(0)
	if previous.Outcome() != protocolsession.SendOutcomeDropped || previous.Identity().LaneID != runtime.initial.ID {
		t.Fatalf("previous=%+v", previous)
	}
}

func TestResponseSequencesDistinguishStreamingResponses(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := newProtocolTraceRecorder(runtime)
	id := id16[protocolsession.OperationID](144)
	request, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &id, []byte{0xa0})
	ctx, _ := testOutboundOperationContext(t, runtime, runtime.initial, request)
	outbound := senderOutbound{runtime: runtime, privateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))}
	fragments, _ := contentflow.FragmentRecord(id, []byte("record"))
	for range 2 {
		_, _ = outbound.executeResponse(ctx, fragments[0].Kind(), id, ProtocolErrorContent{}, func(*outboundTransaction) (outboundLaneAttempt, error) {
			return func(selectedLane, protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
				return protocolsession.SendReceipt{}, errors.New("writer rejected")
			}, nil
		})
	}
	facts := recorder.facts()
	if len(facts) != 2 {
		t.Fatalf("facts=%v", facts)
	}
	first, second := facts[0].(ResponseSendReturned), facts[1].(ResponseSendReturned)
	if first.ResponseSequence() == 0 || second.ResponseSequence() != first.ResponseSequence()+1 || first.Correlation() != second.Correlation() {
		t.Fatalf("facts=%v", facts)
	}
}

func TestResponsePolicySuppressionPreservesEarlierUncertaintyAndCause(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := newProtocolTraceRecorder(runtime)
	base, peer := newMemoryChannelPair()
	defer peer.Close()
	firstIdentity := LaneIdentity{ID: 2, Epoch: 3}
	physicalErr := errors.New("first transport lost acceptance acknowledgment")
	first, err := runtime.lanes.add(firstIdentity, &failingRuntimeLaneChannel{memoryChannel: base, err: physicalErr}, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	initial, _ := runtime.lanes.selectLane(&runtime.initial)
	writerCtx, stop := context.WithCancel(context.Background())
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- first.writer.Run(writerCtx) }()
	go func() { secondDone <- initial.writer.Run(writerCtx) }()
	defer func() { stop(); <-firstDone; <-secondDone }()
	id := id16[protocolsession.OperationID](145)
	request, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &id, []byte{0xa0})
	ctx, _ := testOutboundOperationContext(t, runtime, firstIdentity, request)
	fragments, _ := contentflow.FragmentRecord(id, []byte("record"))
	outbound := senderOutbound{runtime: runtime}
	result, err := outbound.executeResponse(ctx, fragments[0].Kind(), id, ProtocolErrorContent{}, func(transaction *outboundTransaction) (outboundLaneAttempt, error) {
		return func(lane selectedLane, permit protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
			if permit.IsZero() {
				return lane.writer.TryAuthorizedData(fragments[0], transaction.authority)
			}
			if err := runtime.operations.CancelGeneration(transaction.generation); err != nil {
				return protocolsession.SendReceipt{}, err
			}
			return lane.writer.TryDataReplay(fragments[0], permit)
		}, nil
	})
	if err != nil || result.End() != protocolsession.ResponseSendEndPolicySuppressed || result.Evidence() != protocolsession.ResponseSendEvidenceUncertain ||
		result.AttemptCount() != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	firstAttempt, _ := result.Attempt(0)
	secondAttempt, _ := result.Attempt(1)
	if firstAttempt.Identity().LaneID != firstIdentity.ID || firstAttempt.Cause().Kind() != protocolsession.SendAttemptCauseTransportFailure ||
		!strings.Contains(firstAttempt.Cause().Detail(), physicalErr.Error()) || secondAttempt.Outcome() != protocolsession.SendOutcomeDropped ||
		secondAttempt.PolicyAdmitted() || secondAttempt.Cause().Kind() != protocolsession.SendAttemptCauseNone {
		t.Fatalf("attempts=%+v %+v", firstAttempt, secondAttempt)
	}
	facts := recorder.facts()
	if len(facts) != 1 || facts[0].(ResponseSendReturned).Result() != result {
		t.Fatalf("facts=%v", facts)
	}
}
