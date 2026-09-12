package protocolsession

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/observationstream"
)

func settlementCorrelation() SendAttemptCorrelation {
	return SendAttemptCorrelation{
		ProtocolSessionID: ProtocolSessionID{1}, OperationID: testOperationID(1),
		RequestKind: MessageListChildren, ResponseKind: MessageCatalogResult,
		Identity: SendAttemptIdentity{ResponseSequence: 7, AttemptSequence: 2, LaneID: 3, LaneEpoch: 1},
	}
}

func settlementStream(t *testing.T) (observationstream.Producer[SendAttemptSettlement], observationstream.Consumer[SendAttemptSettlement]) {
	t.Helper()
	producer, consumer, err := observationstream.New[SendAttemptSettlement](1)
	if err != nil {
		t.Fatal(err)
	}
	return producer, consumer
}

func TestReceiptSettlementRegistrationAndCompletionShareOnePublication(t *testing.T) {
	for _, lateRegistration := range []bool{false, true} {
		name := "register before settlement"
		if lateRegistration {
			name = "settle before registration"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				producer, consumer := settlementStream(t)
				result := newDeliveryResult()
				receipt := result.receipt()
				correlation := settlementCorrelation()
				if !lateRegistration && !receipt.ObserveSettlement(correlation, producer) {
					t.Fatal("initial registration rejected")
				}
				settledAt := time.Now()
				result.completeTransport(SendOutcomeTransportConfirmed, OutboundReplayPermit{}, false, framechannel.SendAccepted, nil)
				time.Sleep(time.Second)
				if lateRegistration && !receipt.ObserveSettlement(correlation, producer) {
					t.Fatal("late registration rejected")
				}
				if receipt.ObserveSettlement(correlation, producer) {
					t.Fatal("repeated registration acquired publication authority")
				}
				if result.completeTransport(SendOutcomeUnknown, OutboundReplayPermit{}, true, framechannel.SendAccepted, errors.New("too late")) {
					t.Fatal("repeated completion changed settlement")
				}
				cut := producer.Complete()
				if cut.Enqueued != 1 || cut.CapacityDropped != 0 {
					t.Fatalf("publication cut=%+v", cut)
				}
				fact := <-consumer
				if fact.Correlation() != correlation || !fact.ObservedAt().Equal(settledAt) ||
					fact.Attempt().Identity() != correlation.Identity ||
					fact.Attempt().Outcome() != SendOutcomeTransportConfirmed || !fact.Attempt().Settled() {
					t.Fatalf("settlement lost source evidence: %+v", fact)
				}
				if result.settlementObserver != (settlementRegistration{}) {
					t.Fatal("completed receipt retained its observation producer")
				}
				if _, open := <-consumer; open {
					t.Fatal("more than one settlement published")
				}
			})
		})
	}
}

func TestReceiptSettlementConcurrentRegistrationIsExactlyOnce(t *testing.T) {
	const races = 32
	const registrars = 4
	for iteration := range races {
		producer, consumer := settlementStream(t)
		result := newDeliveryResult()
		receipt := result.receipt()
		start := make(chan struct{})
		var work sync.WaitGroup
		accepted := make(chan bool, registrars)
		for range registrars {
			work.Go(func() {
				<-start
				accepted <- receipt.ObserveSettlement(settlementCorrelation(), producer)
			})
		}
		work.Go(func() {
			<-start
			result.completeTransport(SendOutcomeUnknown, OutboundReplayPermit{}, true, framechannel.SendAccepted, errors.New("ambiguous transport"))
		})
		close(start)
		work.Wait()
		close(accepted)
		count := 0
		for claimed := range accepted {
			if claimed {
				count++
			}
		}
		cut := producer.Complete()
		if count != 1 || cut.Enqueued != 1 || cut.CapacityDropped != 0 {
			t.Fatalf("race %d: registrations=%d cut=%+v", iteration, count, cut)
		}
		fact := <-consumer
		if !fact.Attempt().Settled() || fact.Attempt().Outcome() != SendOutcomeUnknown {
			t.Fatalf("settled ambiguity was promoted to confirmation: %+v", fact)
		}
	}
}

func TestReceiptSettlementPublishesEveryCompletionBoundary(t *testing.T) {
	for _, path := range []string{"cancellation before admission", "policy suppression", "late transport rejection", "late transport confirmation", "late transport uncertainty"} {
		t.Run(path, func(t *testing.T) {
			producer, consumer := settlementStream(t)
			result := newDeliveryResult()
			receipt := result.receipt()
			correlation := settlementCorrelation()
			if !receipt.ObserveSettlement(correlation, producer) {
				t.Fatal("registration rejected")
			}
			wantOutcome := SendOutcomeDropped
			switch path {
			case "cancellation before admission":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if completion := receipt.Await(ctx); !completion.Settled || completion.Admitted {
					t.Fatalf("retracted completion=%+v", completion)
				}
			case "policy suppression":
				result.claim()
				result.admit(func() deliveryAdmission { return deliveryAdmission{disposition: OperationDrop} })
			default:
				result.admitBeforeQueue(OutboundAdmission{})
				pending := result.cancelOrSnapshot(context.Canceled)
				if pending.Settled || pending.Outcome != SendOutcomeUnknown {
					t.Fatalf("pending=%+v", pending)
				}
				disposition := framechannel.SendRejected
				cause := errors.New("physical error")
				switch path {
				case "late transport confirmation":
					wantOutcome, disposition, cause = SendOutcomeTransportConfirmed, framechannel.SendAccepted, nil
				case "late transport uncertainty":
					wantOutcome, disposition = SendOutcomeUnknown, framechannel.SendAccepted
				}
				result.completeTransport(wantOutcome, OutboundReplayPermit{}, false, disposition, cause)
			}
			if cut := producer.Complete(); cut.Enqueued != 1 {
				t.Fatalf("cut=%+v", cut)
			}
			fact := <-consumer
			if !fact.Attempt().Settled() || fact.Attempt().Outcome() != wantOutcome {
				t.Fatalf("completion boundary lost facts: %+v", fact)
			}
			if result.settlementObserver != (settlementRegistration{}) {
				t.Fatal("observer retained after completion")
			}
		})
	}
}

func TestReceiptObservationLossNeverChangesCompletion(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		producer, consumer := settlementStream(t)
		if stopped {
			producer.Complete()
		} else {
			producer.TryPublish(SendAttemptSettlement{})
		}
		result := newDeliveryResult()
		receipt := result.receipt()
		if !receipt.ObserveSettlement(settlementCorrelation(), producer) {
			t.Fatal("registration rejected")
		}
		result.completeTransport(SendOutcomeTransportConfirmed, OutboundReplayPermit{}, false, framechannel.SendAccepted, nil)
		completion := receipt.Await(context.Background())
		if !completion.Settled || completion.Outcome != SendOutcomeTransportConfirmed || completion.Err != nil {
			t.Fatalf("observation loss changed completion=%+v", completion)
		}
		cut := producer.Complete()
		if !stopped && (cut.Enqueued != 1 || cut.CapacityDropped != 1) {
			t.Fatalf("bounded stream did not account for loss: %+v", cut)
		}
		if receipt.ObserveSettlement(settlementCorrelation(), producer) {
			t.Fatal("lost publication was retried")
		}
		for range consumer {
		}
	}
}

func TestZeroReceiptAndInvalidCorrelationNeverInventSettlement(t *testing.T) {
	producer, consumer := settlementStream(t)
	receipt := SendReceipt{}
	completion := receipt.Await(context.Background())
	if completion.Settled || completion.Admitted || completion.Outcome != SendOutcomeUninitialized ||
		!errors.Is(completion.Err, ErrWriterStopped) || receipt.Admitted() || receipt.Done() != nil {
		t.Fatalf("zero receipt manufactured an attempt: %+v", completion)
	}
	if receipt.ObserveSettlement(settlementCorrelation(), producer) {
		t.Fatal("zero receipt registered")
	}
	result := newDeliveryResult()
	valid := settlementCorrelation()
	for index := range 5 {
		correlation := valid
		switch index {
		case 0:
			correlation.ProtocolSessionID = ProtocolSessionID{}
		case 1:
			correlation.OperationID = OperationID{}
		case 2:
			correlation.RequestKind = 0
		case 3:
			correlation.ResponseKind = 0
		case 4:
			correlation.Identity = SendAttemptIdentity{}
		}
		if result.receipt().ObserveSettlement(correlation, producer) {
			t.Fatalf("invalid correlation accepted: %+v", correlation)
		}
	}
	if !result.receipt().ObserveSettlement(valid, producer) {
		t.Fatal("invalid registration consumed the one-shot token")
	}
	result.complete(SendOutcomeDropped, OutboundReplayPermit{}, false, nil)
	if cut := producer.Complete(); cut.Enqueued != 1 {
		t.Fatalf("cut=%+v", cut)
	}
	for range consumer {
	}
}

type receiptLockCheckedError struct {
	result *deliveryResult
	detail string
}

func (cause *receiptLockCheckedError) Error() string {
	if !cause.result.mu.TryLock() {
		panic("error formatted under receipt mutex")
	}
	cause.result.mu.Unlock()
	return cause.detail
}

func TestLateSettlementCauseIsFrozenBeforeRegistration(t *testing.T) {
	producer, consumer := settlementStream(t)
	result := newDeliveryResult()
	cause := &receiptLockCheckedError{result: result, detail: "first transport failure"}
	result.completeTransport(SendOutcomeUnknown, OutboundReplayPermit{}, true, framechannel.SendAccepted, cause)
	cause.detail = "later mutation"
	result.receipt().ObserveSettlement(settlementCorrelation(), producer)
	producer.Complete()
	fact := <-consumer
	if fact.Attempt().Cause().Kind() != SendAttemptCauseTransportFailure ||
		fact.Attempt().Cause().Detail() != "first transport failure" {
		t.Fatalf("late registration reread mutable failure: %+v", fact.Attempt().Cause())
	}
}

func TestReceiptInvalidCompletionCannotBePublishedAsPhysicalEvidence(t *testing.T) {
	producer, consumer := settlementStream(t)
	result := newDeliveryResult()
	result.receipt().ObserveSettlement(settlementCorrelation(), producer)
	result.complete(SendOutcomeUninitialized, OutboundReplayPermit{}, false, ErrWriterStopped)
	if cut := producer.Complete(); cut.Enqueued != 0 || cut.CapacityDropped != 0 {
		t.Fatalf("cut=%+v", cut)
	}
	if result.settlementObserver != (settlementRegistration{}) {
		t.Fatal("invalid completion retained observation authority")
	}
	if _, open := <-consumer; open {
		t.Fatal("invalid completion published")
	}
}
