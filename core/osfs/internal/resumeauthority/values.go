// Package resumeauthority reduces durable DirectTree operations after a fresh
// process has reacquired the operation lease and revalidated ownership.
package resumeauthority

import (
	"bytes"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrInvalidContract = errors.New("resume authority contract is invalid")

type EvidenceState uint8

const (
	EvidenceAbsent EvidenceState = iota + 1
	EvidenceProven
	EvidenceUnknown
)

func (state EvidenceState) Valid() bool {
	return state >= EvidenceAbsent && state <= EvidenceUnknown
}

type CleanupEvidenceState uint8

const (
	CleanupPending CleanupEvidenceState = iota + 1
	CleanupComplete
	CleanupUnknown
)

func (state CleanupEvidenceState) Valid() bool {
	return state >= CleanupPending && state <= CleanupUnknown
}

type Snapshot struct {
	operation checkpointmodel.ReceiveOperation
	lifecycle checkpointmodel.ReceiveLifecycleState
}

func NewSnapshot(
	operation checkpointmodel.ReceiveOperation,
	lifecycle checkpointmodel.ReceiveLifecycleState,
) (Snapshot, error) {
	if _, err := operation.VerifyIntent(transfer.DecodeReceiveIntent); err != nil ||
		!lifecycle.Valid() || lifecycle.OperationID() != operation.OperationID() ||
		lifecycle.ReceiveIntentDigest() != operation.ReceiveIntentDigest() {
		return Snapshot{}, errors.Join(ErrInvalidContract, err)
	}
	return Snapshot{operation: operation, lifecycle: lifecycle}, nil
}

// CorruptSnapshot preserves only a structurally decoded immutable operation ID
// for inventory attention. It cannot be acquired or mutated as valid state.
func CorruptSnapshot(operation checkpointmodel.ReceiveOperation) Snapshot {
	if !operation.Valid() {
		return Snapshot{}
	}
	return Snapshot{operation: operation}
}

func (snapshot Snapshot) Operation() checkpointmodel.ReceiveOperation { return snapshot.operation }
func (snapshot Snapshot) Lifecycle() checkpointmodel.ReceiveLifecycleState {
	return snapshot.lifecycle
}
func (snapshot Snapshot) Valid() bool {
	_, err := NewSnapshot(snapshot.operation, snapshot.lifecycle)
	return err == nil
}

type Summary struct {
	operationID receivecontract.OperationID
	intent      transfer.ReceiveIntentDigest
	phase       checkpointmodel.LifecyclePhase
	generation  uint64
	expiresAt   uint64
	successes   uint64
	failures    uint64
	reason      checkpointmodel.NeedsAttentionReason
}

func summaryFromSnapshot(snapshot Snapshot) Summary {
	lifecycle := snapshot.lifecycle
	return Summary{
		operationID: snapshot.operation.OperationID(), intent: snapshot.operation.ReceiveIntentDigest(),
		phase: lifecycle.Phase(), generation: lifecycle.StateGeneration(),
		expiresAt: lifecycle.ExpiresAtMillis(), successes: lifecycle.SuccessCount(),
		failures: lifecycle.FailureCount(), reason: lifecycle.AttentionReason(),
	}
}

func (summary Summary) OperationID() receivecontract.OperationID          { return summary.operationID }
func (summary Summary) ReceiveIntentDigest() transfer.ReceiveIntentDigest { return summary.intent }
func (summary Summary) Phase() checkpointmodel.LifecyclePhase             { return summary.phase }
func (summary Summary) StateGeneration() uint64                           { return summary.generation }
func (summary Summary) ExpiresAtMillis() uint64                           { return summary.expiresAt }
func (summary Summary) SuccessCount() uint64                              { return summary.successes }
func (summary Summary) FailureCount() uint64                              { return summary.failures }
func (summary Summary) NeedsAttentionReason() checkpointmodel.NeedsAttentionReason {
	return summary.reason
}
func (summary Summary) Resumable() bool {
	return summary.phase == checkpointmodel.LifecycleResumableReceive && summary.expiresAt != 0
}

type Attention struct {
	operationID receivecontract.OperationID
	reason      checkpointmodel.NeedsAttentionReason
}

func NewAttention(
	operation receivecontract.OperationID,
	reason checkpointmodel.NeedsAttentionReason,
) (Attention, error) {
	if operation.IsZero() || !reason.Valid() {
		return Attention{}, ErrInvalidContract
	}
	return Attention{operationID: operation, reason: reason}, nil
}

func (attention Attention) OperationID() receivecontract.OperationID { return attention.operationID }
func (attention Attention) Reason() checkpointmodel.NeedsAttentionReason {
	return attention.reason
}

type ListStatus uint8

const (
	ListReady ListStatus = iota + 1
	ListNeedsAttention
)

type Inventory struct {
	status    ListStatus
	summaries []Summary
	attention []Attention
}

func newInventory(summaries []Summary, attention []Attention) Inventory {
	slices.SortFunc(summaries, func(left, right Summary) int {
		return bytes.Compare(left.operationID.Bytes(), right.operationID.Bytes())
	})
	slices.SortFunc(attention, func(left, right Attention) int {
		if compared := bytes.Compare(left.operationID.Bytes(), right.operationID.Bytes()); compared != 0 {
			return compared
		}
		return int(left.reason) - int(right.reason)
	})
	status := ListReady
	if len(attention) != 0 {
		status = ListNeedsAttention
	}
	return Inventory{
		status: status, summaries: slices.Clone(summaries), attention: slices.Clone(attention),
	}
}

func (inventory Inventory) Status() ListStatus     { return inventory.status }
func (inventory Inventory) Summaries() []Summary   { return slices.Clone(inventory.summaries) }
func (inventory Inventory) Attention() []Attention { return slices.Clone(inventory.attention) }

type RecoveryEvidence struct {
	TargetOwnership EvidenceState
	Checkpoints     EvidenceState
	Cleanup         CleanupEvidenceState
	TerminalReceipt checkpointmodel.DirectTreeReceipt
	ExpiryReceipt   checkpointmodel.DirectTreeReceipt
}

func (evidence RecoveryEvidence) valid() bool {
	return evidence.TargetOwnership.Valid() && evidence.Checkpoints.Valid() &&
		evidence.Cleanup.Valid()
}

type DiscardEvidence struct {
	State   CleanupEvidenceState
	Receipt checkpointmodel.DirectTreeReceipt
}

func (evidence DiscardEvidence) valid() bool {
	if !evidence.State.Valid() {
		return false
	}
	return evidence.State != CleanupComplete ||
		evidence.Receipt.Valid() && evidence.Receipt.Kind() == checkpointmodel.ReceiptCleanup
}
