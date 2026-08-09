package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

const nativeReceiptEvidenceDomain = "windshare/native-direct-tree-evidence/v1"

type nativeLifecycleRecorder struct {
	mu sync.Mutex

	authority  *Authority
	intent     transfer.ReceiveIntent
	ownership  checkpointmodel.Ownership
	repository *checkpointstore.Repository
	current    checkpointmodel.ReceiveLifecycleState
}

func newNativeLifecycleRecorder(
	authority *Authority,
	intent transfer.ReceiveIntent,
	ownership checkpointmodel.Ownership,
	repository *checkpointstore.Repository,
) (*nativeLifecycleRecorder, error) {
	if authority == nil || authority.now == nil || intent.IsZero() || !ownership.Valid() || repository == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	current, err := repository.ReadLifecycleState()
	if err != nil {
		return nil, err
	}
	if current.OperationID() != intent.OperationID() || current.ReceiveIntentDigest() != intent.Digest() {
		return nil, checkpointmodel.ErrRecordBinding
	}
	return &nativeLifecycleRecorder{
		authority: authority, intent: intent, ownership: ownership, repository: repository, current: current,
	}, nil
}

func (recorder *nativeLifecycleRecorder) Activate(ctx context.Context) error {
	if recorder == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	switch recorder.current.Phase() {
	case checkpointmodel.LifecycleIntentFrozen, checkpointmodel.LifecycleResumableReceive:
		return recorder.replace(checkpointmodel.LifecycleStateSpec{
			Phase:          checkpointmodel.LifecycleReceiving,
			CheckpointRefs: recorder.current.CheckpointReferences(),
			SuccessCount:   recorder.current.SuccessCount(), FailureCount: recorder.current.FailureCount(),
		})
	case checkpointmodel.LifecycleReceiving, checkpointmodel.LifecycleFinalizingTree:
		if err := recorder.replace(checkpointmodel.LifecycleStateSpec{
			Phase:          checkpointmodel.LifecycleResumableReceive,
			CheckpointRefs: recorder.current.CheckpointReferences(), ExpiresAtMillis: recorder.nextExpiry(),
			SuccessCount: recorder.current.SuccessCount(), FailureCount: recorder.current.FailureCount(),
		}); err != nil {
			return err
		}
		return recorder.replace(checkpointmodel.LifecycleStateSpec{
			Phase:          checkpointmodel.LifecycleReceiving,
			CheckpointRefs: recorder.current.CheckpointReferences(),
			SuccessCount:   recorder.current.SuccessCount(), FailureCount: recorder.current.FailureCount(),
		})
	default:
		return checkpointmodel.ErrInvalidLifecycleState
	}
}

func terminalFileCounts(settlements []transfer.FileSettlement) (uint64, uint64) {
	var successes uint64
	var failures uint64
	for _, settlement := range settlements {
		switch settlement.Kind() {
		case transfer.FilePublished:
			successes++
		case transfer.FilePaused:
			// A paused transaction remains part of the resumable authority graph.
		default:
			failures++
		}
	}
	return successes, failures
}

func (recorder *nativeLifecycleRecorder) RecordTreeSettlement(
	ctx context.Context,
	kind transfer.DirectTreeSettlementKind,
	outcome transfer.DirectTreeOutcome,
	snapshot outputsession.TreeSettlementSnapshot,
) error {
	if recorder == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Paused files carry durable resumable checkpoints, but they are not failed
	// files. Deriving aggregate counts from settlement kinds prevents a normal
	// pause/reopen cycle from poisoning an otherwise complete tree receipt.
	snapshot.SuccessCount, snapshot.FailureCount = terminalFileCounts(snapshot.FileSettlements)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	references, err := recorder.checkpointReferences(snapshot.FileSettlements)
	if err != nil {
		_ = recorder.persistAttention(snapshot, recorder.current.CheckpointReferences())
		return err
	}
	references = mergeCheckpointReferences(recorder.current.CheckpointReferences(), references)
	if kind == transfer.DirectTreeSettlementNeedsAttention {
		return recorder.persistAttention(snapshot, references)
	}
	switch kind {
	case transfer.DirectTreeSettlementResumable:
		return recorder.replace(checkpointmodel.LifecycleStateSpec{
			Phase: checkpointmodel.LifecycleResumableReceive, CheckpointRefs: references,
			ExpiresAtMillis: recorder.nextExpiry(),
			SuccessCount:    max(snapshot.SuccessCount, recorder.current.SuccessCount()),
			FailureCount:    max(snapshot.FailureCount, recorder.current.FailureCount()),
		})
	case transfer.DirectTreeSettlementPublished, transfer.DirectTreeSettlementPartialDirectory:
		err = recorder.finalize(kind, outcome, snapshot, references)
		if err != nil {
			_ = recorder.persistAttention(snapshot, references)
		}
		return err
	default:
		return transfer.ErrInvalidOutputSettlement
	}
}

func (recorder *nativeLifecycleRecorder) finalize(
	kind transfer.DirectTreeSettlementKind,
	outcome transfer.DirectTreeOutcome,
	snapshot outputsession.TreeSettlementSnapshot,
	references []checkpointmodel.FileCheckpointReference,
) error {
	if recorder.current.Phase() != checkpointmodel.LifecycleReceiving ||
		kind == transfer.DirectTreeSettlementPublished && outcome != transfer.DirectTreeOutcomePublished ||
		kind == transfer.DirectTreeSettlementPartialDirectory && outcome != transfer.DirectTreeOutcomePartialDirectory {
		return checkpointmodel.ErrInvalidLifecycleState
	}
	successes := max(snapshot.SuccessCount, recorder.current.SuccessCount())
	failures := max(snapshot.FailureCount, recorder.current.FailureCount())
	if kind == transfer.DirectTreeSettlementPublished && failures != 0 {
		return checkpointmodel.ErrInvalidReceipt
	}
	if err := recorder.replace(checkpointmodel.LifecycleStateSpec{
		Phase: checkpointmodel.LifecycleFinalizingTree, CheckpointRefs: references,
		SuccessCount: successes, FailureCount: failures,
	}); err != nil {
		return err
	}
	if kind == transfer.DirectTreeSettlementPartialDirectory && successes == 0 {
		return recorder.finalizeCollisionOnlyTree(outcome, snapshot, references, failures)
	}
	receiptKind := checkpointmodel.ReceiptTreeCompletion
	partialReason := checkpointmodel.PartialDirectoryReason(0)
	if kind == transfer.DirectTreeSettlementPartialDirectory {
		receiptKind = checkpointmodel.ReceiptPartialDirectory
		partialReason = checkpointmodel.PartialDirectoryFailures
	}
	evidence, err := nativeReceiptEvidence(kind, outcome, references, successes, failures)
	if err != nil {
		return err
	}
	receipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind: receiptKind, OperationID: recorder.intent.OperationID(), ReceiveIntent: recorder.intent.Digest(),
		ReservationDigest: recorder.intent.BindingDigest(), CheckpointRefs: references,
		EvidenceDigest: evidence, SuccessCount: successes, FailureCount: failures, PartialReason: partialReason,
	})
	if err != nil {
		return err
	}
	if err := recorder.repository.InstallReceipt(receipt); err != nil {
		return err
	}
	spec := checkpointmodel.LifecycleStateSpec{
		CheckpointRefs: references, ReceiptDigest: receipt.Digest(),
		SuccessCount: successes, FailureCount: failures,
	}
	if kind == transfer.DirectTreeSettlementPublished {
		spec.Phase = checkpointmodel.LifecyclePublished
		spec.CleanupState = checkpointmodel.OwnedCleanupPending
	} else {
		spec.Phase = checkpointmodel.LifecyclePartialDirectory
		spec.PartialReason = checkpointmodel.PartialDirectoryFailures
	}
	return recorder.replace(spec)
}

func (recorder *nativeLifecycleRecorder) finalizeCollisionOnlyTree(
	outcome transfer.DirectTreeOutcome,
	snapshot outputsession.TreeSettlementSnapshot,
	references []checkpointmodel.FileCheckpointReference,
	failures uint64,
) error {
	if outcome != transfer.DirectTreeOutcomePartialDirectory || len(references) != 0 || failures == 0 {
		return checkpointmodel.ErrInvalidReceipt
	}
	for _, settlement := range snapshot.FileSettlements {
		if settlement.Kind() != transfer.FileCollision {
			return checkpointmodel.ErrInvalidReceipt
		}
	}
	// A collision-only tree owns no file object to retain or delete. Recording a
	// verified no-op cleanup closes the operation as Discarded while the caller
	// still receives the ordinary partial-directory product outcome.
	evidence, err := nativeReceiptEvidence(
		transfer.DirectTreeSettlementPartialDirectory, outcome, nil, 0, failures,
	)
	if err != nil {
		return err
	}
	receipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind: checkpointmodel.ReceiptCleanup, OperationID: recorder.intent.OperationID(),
		ReceiveIntent: recorder.intent.Digest(), ReservationDigest: recorder.intent.BindingDigest(),
		EvidenceDigest: evidence, CleanupGeneration: 1,
	})
	if err != nil {
		return err
	}
	if err := recorder.repository.InstallReceipt(receipt); err != nil {
		return err
	}
	return recorder.replace(checkpointmodel.LifecycleStateSpec{
		Phase: checkpointmodel.LifecycleDiscarded, ReceiptDigest: receipt.Digest(),
		CleanupState: checkpointmodel.OwnedCleanupClean,
	})
}

func (recorder *nativeLifecycleRecorder) persistAttention(
	snapshot outputsession.TreeSettlementSnapshot,
	references []checkpointmodel.FileCheckpointReference,
) error {
	switch recorder.current.Phase() {
	case checkpointmodel.LifecycleIntentFrozen, checkpointmodel.LifecycleReceiving,
		checkpointmodel.LifecycleResumableReceive, checkpointmodel.LifecycleFinalizingTree:
		return recorder.replace(checkpointmodel.LifecycleStateSpec{
			Phase: checkpointmodel.LifecycleNeedsAttention, CheckpointRefs: references,
			SuccessCount:    max(snapshot.SuccessCount, recorder.current.SuccessCount()),
			FailureCount:    max(snapshot.FailureCount, recorder.current.FailureCount()),
			AttentionReason: checkpointmodel.AttentionTargetOwnershipUnknown,
		})
	default:
		return checkpointmodel.ErrInvalidLifecycleState
	}
}

func (recorder *nativeLifecycleRecorder) replace(spec checkpointmodel.LifecycleStateSpec) error {
	if recorder.current.StateGeneration() == math.MaxUint64 {
		return checkpointmodel.ErrInvalidLifecycleState
	}
	spec.OperationID = recorder.intent.OperationID()
	spec.ReceiveIntent = recorder.intent.Digest()
	spec.StateGeneration = recorder.current.StateGeneration() + 1
	next, err := checkpointmodel.NewReceiveLifecycleState(spec)
	if err != nil {
		return err
	}
	if err := recorder.repository.ReplaceLifecycleState(recorder.current, next); err != nil {
		return err
	}
	recorder.current = next
	return nil
}

func (recorder *nativeLifecycleRecorder) nextExpiry() uint64 {
	now := recorder.authority.now().UnixMilli()
	if now < 0 {
		now = 0
	}
	expires, err := checkpointmodel.NextStableExpiry(uint64(now))
	if err != nil {
		return math.MaxUint64
	}
	return expires
}

func (recorder *nativeLifecycleRecorder) checkpointReferences(
	settlements []transfer.FileSettlement,
) ([]checkpointmodel.FileCheckpointReference, error) {
	references := make([]checkpointmodel.FileCheckpointReference, 0, len(settlements))
	for _, settlement := range settlements {
		checkpoint, durable := settlement.VerifiedCheckpoint()
		if !durable {
			continue
		}
		binding := checkpoint.Binding()
		if binding.Locator().Kind() != transfer.MaterializationPathLocator {
			return nil, checkpointmodel.ErrRecordBinding
		}
		ranges := checkpoint.Ranges().Ranges()
		persisted := make([]checkpointmodel.Range, len(ranges))
		for index, item := range ranges {
			persisted[index] = checkpointmodel.Range{Offset: item.Offset, End: item.End}
		}
		record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
			OwnershipMarker: checkpointmodel.OwnershipMarker, Namespace: checkpointmodel.NamespaceName,
			OperationID: recorder.intent.OperationID(), ReceiveIntentDigest: recorder.intent.Digest(),
			MaterializationBindingDigest: recorder.intent.BindingDigest(), FileID: binding.FileID(),
			FileRevision: binding.FileRevision(), CanonicalPath: binding.Locator().CanonicalPath(),
			ExactSize: binding.ExactSize(), MaterializerKind: checkpointmodel.MaterializerNativeTree,
			AuthorityRef: recorder.ownership.AuthorityRef().Bytes(), OwnedObjectID: binding.ObjectIdentity().Bytes(),
			StateGeneration: 1, CheckpointGeneration: uint64(checkpoint.CheckpointGeneration()),
			VerifiedRanges: persisted, Phase: checkpointmodel.PhaseActive, CommitState: checkpointmodel.CommitVerified,
		})
		if err != nil {
			return nil, err
		}
		reference, err := checkpointmodel.NewFileCheckpointReference(record)
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	return references, nil
}

func mergeCheckpointReferences(
	left []checkpointmodel.FileCheckpointReference,
	right []checkpointmodel.FileCheckpointReference,
) []checkpointmodel.FileCheckpointReference {
	merged := make(map[checkpointmodel.RecordID]checkpointmodel.FileCheckpointReference, len(left)+len(right))
	for _, reference := range append(append([]checkpointmodel.FileCheckpointReference(nil), left...), right...) {
		current, exists := merged[reference.RecordID()]
		if !exists || current.CheckpointGeneration() < reference.CheckpointGeneration() {
			merged[reference.RecordID()] = reference
		}
	}
	result := make([]checkpointmodel.FileCheckpointReference, 0, len(merged))
	for _, reference := range merged {
		result = append(result, reference)
	}
	slices.SortFunc(result, func(left, right checkpointmodel.FileCheckpointReference) int {
		if compared := bytes.Compare(left.RecordID().Bytes(), right.RecordID().Bytes()); compared != 0 {
			return compared
		}
		return compareUint64(left.CheckpointGeneration(), right.CheckpointGeneration())
	})
	return result
}

func compareUint64(left, right uint64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func nativeReceiptEvidence(
	kind transfer.DirectTreeSettlementKind,
	outcome transfer.DirectTreeOutcome,
	references []checkpointmodel.FileCheckpointReference,
	successes uint64,
	failures uint64,
) (checkpointmodel.AggregateDigest, error) {
	hash := sha256.New()
	_, _ = hash.Write([]byte(nativeReceiptEvidenceDomain))
	_, _ = hash.Write([]byte{0, byte(kind), byte(outcome)})
	var encoded [8]byte
	for _, value := range []uint64{successes, failures, uint64(len(references))} {
		binary.BigEndian.PutUint64(encoded[:], value)
		_, _ = hash.Write(encoded[:])
	}
	for _, reference := range references {
		_, _ = hash.Write(reference.RecordID().Bytes())
		binary.BigEndian.PutUint64(encoded[:], reference.CheckpointGeneration())
		_, _ = hash.Write(encoded[:])
	}
	digest, err := checkpointmodel.AggregateDigestFromBytes(hash.Sum(nil))
	return digest, err
}
