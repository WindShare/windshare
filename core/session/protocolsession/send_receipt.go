package protocolsession

import (
	"context"
	"sync"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

// SendOutcome distinguishes transport acceptance from proven pre-transport
// suppression. Dropped means the transport did not acquire the frame; Unknown
// cannot prove peer observation and may be either an unsettled caller timeout or
// a settled physical error, as distinguished by SendCompletion.Settled.
type SendOutcome uint8

const (
	SendOutcomeUnknown SendOutcome = iota
	SendOutcomeDelivered
	SendOutcomeDropped
)

// SendReceipt reports actual transport acceptance independently of nonblocking
// queue admission. Await owns cancellation of a not-yet-admitted item, so callers
// must not use concurrent waiters with different cancellation intent. Repeated
// observation after settlement remains safe and returns the stored completion.
type SendReceipt struct{ result *deliveryResult }

type SendCompletion struct {
	Settled             bool
	Admitted            bool
	Outcome             SendOutcome
	Generation          OperationGeneration
	Operation           OutboundOperationPermit
	Replay              OutboundReplayPermit
	RetryableAcrossLane bool
	// Zero means preparation failed before the channel boundary; nonzero records
	// the adapter's exact ownership decision for this physical send.
	TransportDisposition framechannel.SendDisposition
	Err                  error
}

func (receipt SendReceipt) Done() <-chan struct{} {
	if receipt.result == nil {
		return nil
	}
	return receipt.result.done
}

func (receipt SendReceipt) Wait(ctx context.Context) (SendOutcome, error) {
	completion := receipt.Await(ctx)
	return completion.Outcome, completion.Err
}

// Admitted reports whether operation policy has already committed this exact
// message. It is primarily useful for idempotent CANCEL, whose local lifecycle
// completion must not wait for best-effort wire delivery.
func (receipt SendReceipt) Admitted() bool {
	if receipt.result == nil {
		return false
	}
	receipt.result.mu.Lock()
	defer receipt.result.mu.Unlock()
	return receipt.result.phase == deliveryAdmitted ||
		(receipt.result.phase == deliveryCompleted && receipt.result.completionValue.Admitted)
}

// Await atomically retracts an item while the writer can still abandon it without
// consuming wire sequence. Once sealing begins or operation policy admits the
// message, cancellation can no longer prove a pre-transport drop; the caller
// receives an unsettled Unknown completion while the writer publishes the result.
func (receipt SendReceipt) Await(ctx context.Context) SendCompletion {
	if receipt.result == nil {
		return SendCompletion{Settled: true, Outcome: SendOutcomeUnknown, Err: ErrWriterStopped}
	}
	select {
	case <-receipt.result.done:
		return receipt.result.completion()
	case <-ctx.Done():
		return receipt.result.cancelOrSnapshot(ctx.Err())
	}
}

type deliveryPhase uint8

const (
	deliveryPending deliveryPhase = iota
	deliveryReserved
	deliveryReservedClaimed
	deliveryReservedSealing
	deliveryClaimed
	deliveryAdmitted
	deliveryCompleted
)

type deliveryAdmission struct {
	disposition  OperationDisposition
	generation   OperationGeneration
	operation    OutboundOperationPermit
	replay       OutboundReplayPermit
	pin          *outboundAdmissionPin
	continuation *operationContinuationReservation
	admitted     bool
	err          error
}

type deliveryResult struct {
	mu                   sync.Mutex
	phase                deliveryPhase
	done                 chan struct{}
	completionValue      SendCompletion
	settlementLease      *OutboundOperationLease
	reservedContinuation *operationContinuationReservation
	reservedPin          *outboundAdmissionPin
}

func newDeliveryResult() *deliveryResult            { return &deliveryResult{done: make(chan struct{})} }
func (result *deliveryResult) receipt() SendReceipt { return SendReceipt{result: result} }

func (result *deliveryResult) claim() (claimed bool, policyPrepared bool, alreadyAdmitted bool) {
	result.mu.Lock()
	defer result.mu.Unlock()
	switch result.phase {
	case deliveryPending:
		result.phase = deliveryClaimed
		return true, false, false
	case deliveryReserved:
		result.phase = deliveryReservedClaimed
		return true, true, false
	case deliveryAdmitted:
		// Terminal and CANCEL admission can occur synchronously before queue
		// publication; delivery must not run operation policy a second time.
		return true, true, true
	default:
		return false, false, false
	}
}

func (result *deliveryResult) beginReservationSeal() bool {
	result.mu.Lock()
	defer result.mu.Unlock()
	if result.phase != deliveryReservedClaimed {
		return false
	}
	// Seal may advance the lane sequence before returning. From this boundary the
	// writer must retain the reservation so caller cancellation cannot create an
	// unsent sequence gap while leaving the lane usable.
	result.phase = deliveryReservedSealing
	return true
}

func (result *deliveryResult) commitReservationSeal() error {
	result.mu.Lock()
	defer result.mu.Unlock()
	if result.phase != deliveryReservedSealing {
		return ErrSealingReservation
	}
	// Publish Admitted and replay authority only after the continuation record is
	// committed. Holding the result lock gives Await one coherent snapshot, while
	// the result-to-operation-table lock order matches admission and rollback.
	result.reservedContinuation.commit()
	result.phase = deliveryAdmitted
	result.completionValue.Admitted = true
	result.reservedContinuation = nil
	result.reservedPin = nil
	return nil
}

func (result *deliveryResult) admit(decide func() deliveryAdmission) (deliveryAdmission, bool) {
	result.mu.Lock()
	defer result.mu.Unlock()
	if result.phase != deliveryClaimed {
		return deliveryAdmission{}, false
	}
	// Holding this mutex across lifecycle admission gives Await a single total
	// order: cancellation either retracts before policy mutation or observes the
	// admitted authority and must remain uncertain.
	decision := decide()
	if decision.err != nil || decision.disposition != OperationDeliver {
		decision.pin.release()
		decision.continuation.rollback()
		result.completeLocked(SendCompletion{
			Settled: true, Admitted: decision.admitted, Outcome: SendOutcomeDropped,
			Generation: decision.generation, Operation: decision.operation,
			Replay: decision.replay, Err: decision.err,
		})
		return decision, true
	}
	result.phase = deliveryAdmitted
	result.completionValue.Admitted = true
	result.completionValue.Generation = decision.generation
	result.completionValue.Operation = decision.operation
	result.completionValue.Replay = decision.replay
	return decision, true
}

func (result *deliveryResult) admitBeforeQueue(admission OutboundAdmission) bool {
	result.mu.Lock()
	defer result.mu.Unlock()
	if result.phase != deliveryPending {
		return false
	}
	result.phase = deliveryAdmitted
	result.completionValue.Admitted = true
	result.completionValue.Generation = admission.Generation
	result.completionValue.Operation = admission.Operation
	result.completionValue.Replay = admission.Replay
	return true
}

func (result *deliveryResult) reserveBeforeQueue(admission OutboundAdmission) bool {
	result.mu.Lock()
	defer result.mu.Unlock()
	if result.phase != deliveryPending {
		return false
	}
	result.phase = deliveryReserved
	result.completionValue.Generation = admission.Generation
	result.completionValue.Operation = admission.Operation
	result.completionValue.Replay = admission.Replay
	result.reservedContinuation = admission.continuation
	result.reservedPin = admission.pin
	return true
}

func (result *deliveryResult) complete(
	outcome SendOutcome,
	replay OutboundReplayPermit,
	retryableAcrossLane bool,
	err error,
) bool {
	return result.completeTransport(outcome, replay, retryableAcrossLane, 0, err)
}

func (result *deliveryResult) completeTransport(
	outcome SendOutcome,
	replay OutboundReplayPermit,
	retryableAcrossLane bool,
	transportDisposition framechannel.SendDisposition,
	err error,
) bool {
	if outcome != SendOutcomeUnknown && replay.direction == DirectionReceiverToSender && replay.kind.isRequest() {
		// Receiver request replay exists only to resolve a settled ambiguous send.
		// Proven delivery or pre-transport drop must not export reusable authority.
		replay = OutboundReplayPermit{}
	}
	result.mu.Lock()
	defer result.mu.Unlock()
	if result.phase == deliveryCompleted {
		return false
	}
	admitted := result.phase == deliveryAdmitted
	result.rollbackReservationLocked()
	generation := result.completionValue.Generation
	operation := result.completionValue.Operation
	result.completeLocked(SendCompletion{
		Settled: true, Admitted: admitted, Outcome: outcome, Replay: replay,
		Generation: generation, Operation: operation,
		RetryableAcrossLane:  retryableAcrossLane,
		TransportDisposition: transportDisposition,
		Err:                  err,
	})
	return true
}

func (result *deliveryResult) completeLocked(completion SendCompletion) {
	result.rollbackReservationLocked()
	result.phase = deliveryCompleted
	result.completionValue = completion
	close(result.done)
	if result.settlementLease != nil {
		result.settlementLease.Release()
		result.settlementLease = nil
	}
}

func (result *deliveryResult) rollbackReservationLocked() {
	result.reservedContinuation.rollback()
	result.reservedContinuation = nil
	result.reservedPin.release()
	result.reservedPin = nil
}

// ReleaseLeaseOnSettlement transfers a generation lease from a caller that can
// no longer wait to the writer's eventual physical completion.
func (receipt SendReceipt) ReleaseLeaseOnSettlement(lease *OutboundOperationLease) {
	if lease == nil {
		return
	}
	if receipt.result == nil {
		lease.Release()
		return
	}
	receipt.result.mu.Lock()
	if receipt.result.phase == deliveryCompleted {
		receipt.result.mu.Unlock()
		lease.Release()
		return
	}
	if receipt.result.settlementLease != nil {
		receipt.result.mu.Unlock()
		// A receipt represents one physical settlement, so retaining multiple
		// generation leases would let repeated attachment grow unboundedly.
		lease.Release()
		return
	}
	receipt.result.settlementLease = lease
	receipt.result.mu.Unlock()
}

func (result *deliveryResult) cancelOrSnapshot(cause error) SendCompletion {
	result.mu.Lock()
	defer result.mu.Unlock()
	switch result.phase {
	case deliveryPending, deliveryReserved, deliveryReservedClaimed, deliveryClaimed:
		result.completeLocked(SendCompletion{
			Settled: true, Outcome: SendOutcomeDropped, Err: cause,
		})
		return result.completionValue
	case deliveryReservedSealing:
		return result.uncertainSnapshotLocked(false, cause)
	case deliveryAdmitted:
		return result.uncertainSnapshotLocked(true, cause)
	default:
		return result.completionValue
	}
}

func (result *deliveryResult) uncertainSnapshotLocked(admitted bool, cause error) SendCompletion {
	var replay OutboundReplayPermit
	if admitted {
		replay = result.completionValue.Replay
		if replay.direction == DirectionReceiverToSender && replay.kind.isRequest() {
			// Caller cancellation is not a settled transport result. Keep exact
			// operation authority for cleanup without exporting request replay.
			replay = OutboundReplayPermit{}
		}
	}
	return SendCompletion{
		Admitted: admitted, Outcome: SendOutcomeUnknown,
		Generation: result.completionValue.Generation,
		Operation:  result.completionValue.Operation,
		Replay:     replay, Err: cause,
	}
}

func (result *deliveryResult) completion() SendCompletion {
	result.mu.Lock()
	defer result.mu.Unlock()
	return result.completionValue
}
