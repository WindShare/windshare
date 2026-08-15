package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type OperationPageCursor struct {
	after string
}

func NewOperationPageCursor(after receivecontract.OperationID) OperationPageCursor {
	if after.IsZero() {
		return OperationPageCursor{}
	}
	return OperationPageCursor{after: operationNamespaceName(after)}
}

func (cursor OperationPageCursor) IsZero() bool { return cursor.after == "" }

func (cursor OperationPageCursor) After() (receivecontract.OperationID, bool) {
	if cursor.after == "" {
		return receivecontract.OperationID{}, false
	}
	operation, err := parseOperationNamespaceName(cursor.after)
	return operation, err == nil
}

type OperationPage struct {
	records []checkpointmodel.OrdinaryOperationRecord
	next    OperationPageCursor
	unknown bool
}

func (page OperationPage) Records() []checkpointmodel.OrdinaryOperationRecord {
	return slices.Clone(page.records)
}
func (page OperationPage) Next() OperationPageCursor { return page.next }
func (page OperationPage) Unknown() bool             { return page.unknown }

func (registry *OperationRegistry) PageOperations(
	cursor OperationPageCursor,
	maximum int,
) (OperationPage, error) {
	if !registry.valid() || maximum <= 0 || maximum > MaximumOrdinaryOperationPageSizeV1 {
		return OperationPage{}, transfer.ErrInvalidOutputBinding
	}
	// Names is deliberately bounded per call. The native API has no cursor, so
	// callers page the sorted prefix; operation cardinality is bounded by the
	// explicit request rather than by file-checkpoint count.
	upperBound := MaximumOrdinaryOperationRecordsV1 + 1
	names, err := registry.operations.Names(upperBound)
	if err != nil {
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			return OperationPage{unknown: true}, nil
		}
		return OperationPage{}, repositoryError("page ordinary operations", err)
	}
	slices.Sort(names)
	start := 0
	if cursor.after != "" {
		start, _ = slices.BinarySearch(names, cursor.after)
		for start < len(names) && names[start] <= cursor.after {
			start++
		}
	}
	page := OperationPage{}
	end := min(start+maximum, len(names))
	for _, name := range names[start:end] {
		operation, parseErr := parseOperationNamespaceName(name)
		if parseErr != nil {
			page.unknown = true
			continue
		}
		record, _, readErr := registry.readOperation(operation)
		if readErr != nil {
			page.unknown = true
			continue
		}
		page.records = append(page.records, record)
	}
	if end < len(names) {
		page.next = OperationPageCursor{after: names[end-1]}
	}
	return page, nil
}

func (registry *OperationRegistry) readOperation(
	operation receivecontract.OperationID,
) (checkpointmodel.OrdinaryOperationRecord, []byte, error) {
	directory, err := openExistingDirectory(registry.operations, operationNamespaceName(operation))
	if err != nil {
		return checkpointmodel.OrdinaryOperationRecord{}, nil, repositoryError("open ordinary operation", err)
	}
	encoded, readErr := ReadFile(directory, ordinaryOperationRecordFile)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return checkpointmodel.OrdinaryOperationRecord{}, nil, repositoryError("read ordinary operation", errors.Join(readErr, closeErr))
	}
	record, decodeErr := checkpointmodel.DecodeOrdinaryOperationRecord(encoded)
	if decodeErr != nil || record.OperationID() != operation {
		return checkpointmodel.OrdinaryOperationRecord{}, nil, codedError(ErrorCorruptRecord, "decode ordinary operation", errors.Join(decodeErr, checkpointmodel.ErrInvalidOrdinaryOperation))
	}
	if _, verifyErr := record.VerifyIntent(transfer.DecodeReceiveIntent); verifyErr != nil {
		return checkpointmodel.OrdinaryOperationRecord{}, nil, codedError(ErrorCorruptRecord, "verify ordinary operation intent", verifyErr)
	}
	return record, encoded, nil
}

func (registry *OperationRegistry) replaceOperation(
	previous checkpointmodel.OrdinaryOperationRecord,
	next checkpointmodel.OrdinaryOperationRecord,
) error {
	directory, err := openExistingDirectory(registry.operations, operationNamespaceName(previous.OperationID()))
	if err != nil {
		return repositoryError("open ordinary operation replacement", err)
	}
	previousBytes, previousErr := checkpointmodel.EncodeOrdinaryOperationRecord(previous)
	nextBytes, nextErr := checkpointmodel.EncodeOrdinaryOperationRecord(next)
	if previousErr != nil || nextErr != nil {
		return errors.Join(codedError(ErrorCorruptRecord, "encode ordinary operation replacement", errors.Join(previousErr, nextErr)), directory.Close())
	}
	replaceErr := InstallReplace(directory, ordinaryOperationRecordFile, previousBytes, nextBytes)
	return repositoryError("replace ordinary operation", errors.Join(replaceErr, directory.Close()))
}

func (registry *OperationRegistry) removeActiveIndex(record checkpointmodel.OrdinaryOperationRecord) error {
	directory, err := openExistingDirectory(registry.active, activeKeyName(record.ActiveOperationKey()))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return repositoryError("open ordinary active de-index", err)
	}
	bindingDigest, digestErr := checkpointmodel.OrdinaryOperationBindingDigest(record)
	removeErr, closeFileErr := RemoveExact(directory, operationNamespaceName(record.OperationID()), bindingDigest[:])
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return repositoryError("remove ordinary active index", errors.Join(digestErr, removeErr, closeFileErr, directory.Close()))
}

func validActiveIndexDigest(
	indexDigest []byte,
	record checkpointmodel.OrdinaryOperationRecord,
	encoded []byte,
) bool {
	bindingDigest, err := checkpointmodel.OrdinaryOperationBindingDigest(record)
	return err == nil && len(indexDigest) == sha256.Size && bytes.Equal(indexDigest, bindingDigest[:]) && len(encoded) > 0
}

func (registry *OperationRegistry) lookupAdmissionCandidate(
	key checkpointmodel.ActiveOperationKey,
) (ActiveLookup, error) {
	directory, err := openExistingDirectory(registry.candidates, activeKeyName(key))
	if errors.Is(err, fs.ErrNotExist) {
		return ActiveLookup{state: ActiveLookupNone}, nil
	}
	if err != nil {
		return ActiveLookup{state: ActiveLookupAmbiguous}, nil
	}
	encoded, readErr := ReadFile(directory, ordinaryAdmissionCandidateFile)
	entriesErr := validateAllowedEntries(directory, ordinaryAdmissionCandidateEntries)
	closeErr := directory.Close()
	if errors.Is(readErr, fs.ErrNotExist) && entriesErr == nil && closeErr == nil {
		return ActiveLookup{state: ActiveLookupNone}, nil
	}
	candidate, decodeErr := checkpointmodel.DecodeOrdinaryAdmissionCandidate(encoded)
	if readErr != nil || entriesErr != nil || closeErr != nil || decodeErr != nil ||
		candidate.ActiveOperationKey() != key {
		return ActiveLookup{state: ActiveLookupAmbiguous}, nil
	}
	// Preparing is not a reopenable operation: it may be a clean crash candidate
	// or may have crossed a public mutation cut. Only explicit reconciliation can
	// retire it, so ordinary admission stops rather than guessing.
	return ActiveLookup{state: ActiveLookupNeedsAttention}, nil
}

func (registry *OperationRegistry) removeAdmissionCandidate(
	candidate checkpointmodel.OrdinaryAdmissionCandidate,
) error {
	if !registry.valid() || !candidate.Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	directory, err := openExistingDirectory(registry.candidates, activeKeyName(candidate.ActiveOperationKey()))
	if err != nil {
		return repositoryError("open ordinary admission candidate removal", err)
	}
	encoded, encodeErr := checkpointmodel.EncodeOrdinaryAdmissionCandidate(candidate)
	removeErr, closeFileErr := RemoveExact(directory, ordinaryAdmissionCandidateFile, encoded)
	return repositoryError("remove ordinary admission candidate", errors.Join(encodeErr, removeErr, closeFileErr, directory.Close()))
}

func (registry *OperationRegistry) removeOperationCandidate(
	record checkpointmodel.OrdinaryOperationRecord,
) error {
	if !registry.valid() || !record.Valid() || record.Lifecycle().ParticipatesInActiveLookup() {
		return transfer.ErrInvalidOutputBinding
	}
	directory, err := openExistingDirectory(registry.candidates, activeKeyName(record.ActiveOperationKey()))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return repositoryError("open terminal ordinary admission candidate", err)
	}
	encoded, readErr := ReadFile(directory, ordinaryAdmissionCandidateFile)
	if errors.Is(readErr, fs.ErrNotExist) {
		entriesErr := validateAllowedEntries(directory, ordinaryAdmissionCandidateEntries)
		return repositoryError("authenticate empty terminal ordinary admission candidate namespace",
			errors.Join(entriesErr, directory.Close()))
	}
	candidate, decodeErr := checkpointmodel.DecodeOrdinaryAdmissionCandidate(encoded)
	if readErr != nil || decodeErr != nil || candidate.ActiveOperationKey() != record.ActiveOperationKey() ||
		candidate.OperationID() != record.OperationID() || !candidate.ReservationClaim().Valid() ||
		candidate.ReservationClaim().Token() != record.ReservationClaim().Token() {
		return repositoryError("authenticate terminal ordinary admission candidate", errors.Join(
			readErr, decodeErr, checkpointmodel.ErrInvalidOrdinaryOperation, directory.Close(),
		))
	}
	// The operation-bound proof authenticates the candidate's exact name claim
	// against the immutable row before terminal cleanup may delete metadata.
	if _, proofErr := registry.recoveryProof(record); proofErr != nil {
		return errors.Join(proofErr, directory.Close())
	}
	intent, intentErr := record.VerifyIntent(transfer.DecodeReceiveIntent)
	reservation, direct := intent.MaterializationPlan().DestinationReservation()
	claimAdvance := singleFileCandidateClaimAdvanceV1
	if reservation.EntryKind() == receivecontract.ContainerEntryResultRoot {
		claimAdvance = resultRootCandidateClaimAdvanceV1
	}
	candidateGeneration := candidate.ReservationClaim().Generation()
	if intentErr != nil || !direct || candidateGeneration > ^uint64(0)-claimAdvance ||
		candidateGeneration+claimAdvance != record.ReservationClaim().Generation() {
		return repositoryError("authenticate terminal ordinary admission candidate claim",
			errors.Join(intentErr, checkpointmodel.ErrInvalidOrdinaryOperation, directory.Close()))
	}
	if entriesErr := validateAllowedEntries(directory, ordinaryAdmissionCandidateEntries); entriesErr != nil {
		return repositoryError("authenticate terminal ordinary admission candidate namespace",
			errors.Join(entriesErr, directory.Close()))
	}
	removeErr, closeFileErr := RemoveExact(directory, ordinaryAdmissionCandidateFile, encoded)
	return repositoryError("remove terminal ordinary admission candidate", errors.Join(removeErr, closeFileErr, directory.Close()))
}

func validateOrdinaryRecordTransition(
	previous checkpointmodel.OrdinaryOperationRecord,
	next checkpointmodel.OrdinaryOperationRecord,
) error {
	if previous.Lease() != checkpointmodel.OrdinaryLeaseHeld {
		return checkpointmodel.ErrInvalidOrdinaryLifecycle
	}
	if next.Lease() != checkpointmodel.OrdinaryLeaseHeld && next.Lease() != checkpointmodel.OrdinaryLeaseReleased {
		return checkpointmodel.ErrInvalidOrdinaryLifecycle
	}
	if previous.Lifecycle() == next.Lifecycle() && previous.ClosedReason() == next.ClosedReason() {
		return nil
	}
	for _, event := range []checkpointmodel.OrdinaryLifecycleEvent{
		checkpointmodel.OrdinaryLifecycleContinue, checkpointmodel.OrdinaryLifecycleRequireAttention,
		checkpointmodel.OrdinaryLifecycleComplete, checkpointmodel.OrdinaryLifecycleDiscard,
		checkpointmodel.OrdinaryLifecycleCleanupFailed,
	} {
		state, reason, err := checkpointmodel.ReduceOrdinaryOperationLifecycle(previous.Lifecycle(), event, next.ClosedReason())
		if err == nil && state == next.Lifecycle() && reason == next.ClosedReason() {
			return nil
		}
	}
	return checkpointmodel.ErrInvalidOrdinaryLifecycle
}

func (registry *OperationRegistry) valid() bool {
	return registry != nil && registry.root != nil && registry.operations != nil && registry.active != nil &&
		registry.leases != nil && registry.claims != nil && registry.candidates != nil
}

func operationNamespaceName(operation receivecontract.OperationID) string {
	return hex.EncodeToString(operation.Bytes())
}

func parseOperationNamespaceName(name string) (receivecontract.OperationID, error) {
	if len(name) != receivecontract.StableIdentityBytes*2 || name != strings.ToLower(name) {
		return receivecontract.OperationID{}, checkpointmodel.ErrRecordBinding
	}
	raw, err := hex.DecodeString(name)
	if err != nil {
		return receivecontract.OperationID{}, checkpointmodel.ErrRecordBinding
	}
	return receivecontract.OperationIDFromBytes(raw)
}

func activeKeyName(key checkpointmodel.ActiveOperationKey) string {
	return hex.EncodeToString(key.Bytes())
}

func activeLockName(key checkpointmodel.ActiveOperationKey) string {
	return activeKeyName(key) + ordinaryActiveLockSuffix
}

func operationLeaseNameV1(operation receivecontract.OperationID) string {
	return operationNamespaceName(operation) + ordinaryOperationLockSuffix
}

func claimLockName(token [sha256.Size]byte) string {
	return hex.EncodeToString(token[:]) + ordinaryClaimLockSuffix
}

func sameOrdinaryRecord(left, right checkpointmodel.OrdinaryOperationRecord) bool {
	leftEncoded, leftErr := checkpointmodel.EncodeOrdinaryOperationRecord(left)
	rightEncoded, rightErr := checkpointmodel.EncodeOrdinaryOperationRecord(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftEncoded, rightEncoded)
}
