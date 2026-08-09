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
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type CompatibleLookup struct {
	operations         []checkpointmodel.ReceiveOperation
	ownershipUncertain bool
}

func (lookup CompatibleLookup) Operations() []checkpointmodel.ReceiveOperation {
	return slices.Clone(lookup.operations)
}

func (lookup CompatibleLookup) OwnershipUncertain() bool { return lookup.ownershipUncertain }

// InstallOperation publishes the immutable binding last. A restart can safely
// treat a reservation without the operation file as incomplete rather than as
// authority for reopening the target.
func (repository *Repository) InstallOperation(
	record checkpointmodel.ReceiveOperation,
	materializationBinding []byte,
) error {
	if repository == nil || repository.operation == nil || !record.Valid() ||
		record.OperationID() != repository.binding.OperationID() ||
		record.ReceiveIntentDigest() != repository.binding.ReceiveIntentDigest() ||
		record.BindingDigest() != repository.binding.MaterializationBindingDigest() ||
		len(materializationBinding) == 0 || len(materializationBinding) > maxRepositoryRecordBytes {
		return transfer.ErrInvalidOutputBinding
	}
	if receivecontract.BindingDigest(sha256.Sum256(materializationBinding)) != record.BindingDigest() {
		return codedError(ErrorCorruptRecord, "bind operation materialization", checkpointmodel.ErrRecordBinding)
	}
	if _, err := record.VerifyIntent(transfer.DecodeReceiveIntent); err != nil {
		return codedError(ErrorCorruptRecord, "verify receive operation", err)
	}
	encoded, err := checkpointmodel.EncodeReceiveOperation(record)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode receive operation", err)
	}
	if err := InstallCreate(repository.operation, ReservationFile, materializationBinding); err != nil {
		return repositoryError("install operation reservation", err)
	}
	if err := InstallCreate(repository.operation, OperationFile, encoded); err != nil {
		return repositoryError("install receive operation", err)
	}
	return nil
}

func (repository *Repository) ReadOperation() (checkpointmodel.ReceiveOperation, error) {
	if repository == nil || repository.operation == nil {
		return checkpointmodel.ReceiveOperation{}, transfer.ErrInvalidOutputBinding
	}
	encoded, err := ReadFile(repository.operation, OperationFile)
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, repositoryError("read receive operation", err)
	}
	record, err := checkpointmodel.DecodeReceiveOperation(encoded)
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, codedError(ErrorCorruptRecord, "decode receive operation", err)
	}
	if _, err := record.VerifyIntent(transfer.DecodeReceiveIntent); err != nil {
		return checkpointmodel.ReceiveOperation{}, codedError(ErrorCorruptRecord, "verify receive operation", err)
	}
	if record.OperationID() != repository.binding.OperationID() ||
		record.ReceiveIntentDigest() != repository.binding.ReceiveIntentDigest() ||
		record.BindingDigest() != repository.binding.MaterializationBindingDigest() {
		return checkpointmodel.ReceiveOperation{}, codedError(
			ErrorCorruptRecord, "bind receive operation", checkpointmodel.ErrRecordBinding,
		)
	}
	return record, nil
}

func (repository *Repository) ReadMaterializationBinding() ([]byte, error) {
	if repository == nil || repository.operation == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	encoded, err := ReadFile(repository.operation, ReservationFile)
	if err != nil {
		return nil, repositoryError("read operation reservation", err)
	}
	if receivecontract.BindingDigest(sha256.Sum256(encoded)) !=
		repository.binding.MaterializationBindingDigest() {
		return nil, codedError(ErrorCorruptRecord, "bind operation reservation", checkpointmodel.ErrRecordBinding)
	}
	return encoded, nil
}

func (lease *OperationLease) RegisterLookup(record checkpointmodel.ReceiveOperation) error {
	if lease == nil || lease.operations == nil || lease.lookup == nil || lease.lock == nil ||
		!record.Valid() || record.OperationID() != lease.operation ||
		record.ReceiveIntentDigest() != lease.binding.ReceiveIntentDigest() ||
		record.BindingDigest() != lease.binding.MaterializationBindingDigest() ||
		record.ReopenKey().Kind() != checkpointmodel.ReopenCLICompatible {
		return transfer.ErrInvalidOutputBinding
	}
	repository, err := lease.OpenExistingRepository()
	if err != nil {
		return repositoryError("open operation repository for lookup", err)
	}
	stored, operationErr := repository.ReadOperation()
	reservationBytes, reservationErr := repository.ReadMaterializationBinding()
	lifecycle, lifecycleErr := repository.ReadLifecycleState()
	closeErr := repository.Close()
	if operationErr != nil || reservationErr != nil || lifecycleErr != nil || closeErr != nil {
		return repositoryError(
			"read operation for lookup",
			errors.Join(operationErr, reservationErr, lifecycleErr, closeErr),
		)
	}
	intent, verifyErr := stored.VerifyIntent(transfer.DecodeReceiveIntent)
	reservation, direct := intent.MaterializationPlan().DestinationReservation()
	ownership := lease.binding.Ownership()
	candidate, knownState := directTreeLookupState(lifecycle)
	if verifyErr != nil || !sameOperationRecord(stored, record) || !direct || !candidate || !knownState ||
		intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree ||
		reservation.Digest() != stored.BindingDigest() ||
		reservation.AuthorityRef() != ownership.AuthorityRef() ||
		ownership.MaterializerKind() != checkpointmodel.MaterializerNativeTree ||
		!bytes.Equal(reservation.CanonicalBytes(), reservationBytes) {
		return codedError(
			ErrorCorruptRecord, "verify operation for lookup",
			errors.Join(verifyErr, checkpointmodel.ErrRecordBinding),
		)
	}
	encoded, err := checkpointmodel.EncodeReceiveOperation(stored)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode operation for lookup", err)
	}
	keyDirectory, err := openOrCreateDirectory(
		lease.lookup, hex.EncodeToString(record.ReopenKey().CompatibleKey().Bytes()),
	)
	if err != nil {
		return repositoryError("open compatible operation lookup", err)
	}
	digest := sha256.Sum256(encoded)
	installErr := InstallCreate(keyDirectory, operationNamespaceName(lease.operation), digest[:])
	return repositoryError("install compatible operation lookup", errors.Join(installErr, keyDirectory.Close()))
}

// LookupCompatible verifies every candidate against both its index digest and
// immutable operation record. Unknown entries are retained as uncertainty so a
// caller can project target-ownership-unknown instead of guessing one winner.
func (namespace *Namespace) LookupCompatible(key checkpointmodel.CompatibleOperationKey) (CompatibleLookup, error) {
	if namespace == nil || namespace.lookup == nil || namespace.operations == nil || key.IsZero() {
		return CompatibleLookup{}, transfer.ErrInvalidOutputBinding
	}
	keyDirectory, err := openExistingDirectory(namespace.lookup, hex.EncodeToString(key.Bytes()))
	if errors.Is(err, fs.ErrNotExist) {
		return CompatibleLookup{}, nil
	}
	if err != nil {
		return CompatibleLookup{}, repositoryError("open compatible operation lookup", err)
	}
	defer keyDirectory.Close()
	names, err := keyDirectory.Names(checkpointmodel.MaxCheckpointRecordsPerOperation + 1)
	if err != nil {
		return CompatibleLookup{}, repositoryError("list compatible operations", err)
	}
	if len(names) > checkpointmodel.MaxCheckpointRecordsPerOperation {
		return CompatibleLookup{ownershipUncertain: true}, nil
	}
	slices.Sort(names)
	result := CompatibleLookup{}
	for _, name := range names {
		operationID, parseErr := parseOperationNamespaceName(name)
		if parseErr != nil {
			result.ownershipUncertain = true
			continue
		}
		indexDigest, readErr := ReadFile(keyDirectory, name)
		if readErr != nil || len(indexDigest) != sha256.Size {
			result.ownershipUncertain = true
			continue
		}
		record, candidate, uncertain := namespace.inspectCompatibleOperation(
			operationID, name, indexDigest, key,
		)
		result.ownershipUncertain = result.ownershipUncertain || uncertain
		if candidate {
			result.operations = append(result.operations, record)
		}
	}
	return result, nil
}

func (namespace *Namespace) inspectCompatibleOperation(
	operationID receivecontract.OperationID,
	name string,
	indexDigest []byte,
	key checkpointmodel.CompatibleOperationKey,
) (checkpointmodel.ReceiveOperation, bool, bool) {
	operationDirectory, err := openExistingDirectory(namespace.operations, name)
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, false, true
	}
	encoded, readErr := ReadFile(operationDirectory, OperationFile)
	closeErr := operationDirectory.Close()
	indexed, decodeErr := checkpointmodel.DecodeReceiveOperation(encoded)
	if readErr != nil || closeErr != nil || decodeErr != nil ||
		!bytes.Equal(indexDigest, sha256Bytes(encoded)) || indexed.OperationID() != operationID ||
		indexed.ReopenKey().Kind() != checkpointmodel.ReopenCLICompatible ||
		indexed.ReopenKey().CompatibleKey() != key {
		return checkpointmodel.ReceiveOperation{}, false, true
	}
	lease, err := namespace.AcquireOperation(
		operationID, indexed.ReceiveIntentDigest(), indexed.BindingDigest(),
	)
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, false, true
	}
	repository, err := lease.OpenExistingRepository()
	if err != nil {
		_ = lease.Close()
		return checkpointmodel.ReceiveOperation{}, false, true
	}
	record, operationErr := repository.ReadOperation()
	reservationBytes, reservationErr := repository.ReadMaterializationBinding()
	lifecycle, lifecycleErr := repository.ReadLifecycleState()
	repositoryCloseErr := repository.Close()
	leaseCloseErr := lease.Close()
	canonical, canonicalErr := checkpointmodel.EncodeReceiveOperation(record)
	if operationErr != nil || reservationErr != nil || lifecycleErr != nil ||
		repositoryCloseErr != nil || leaseCloseErr != nil || canonicalErr != nil ||
		!sameOperationRecord(record, indexed) || !bytes.Equal(indexDigest, sha256Bytes(canonical)) {
		return checkpointmodel.ReceiveOperation{}, false, true
	}
	intent, verifyErr := record.VerifyIntent(transfer.DecodeReceiveIntent)
	reservation, direct := intent.MaterializationPlan().DestinationReservation()
	if verifyErr != nil || !direct ||
		intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree ||
		reservation.OperationID() != operationID || reservation.Digest() != record.BindingDigest() ||
		reservation.AuthorityRef() != namespace.ownership.AuthorityRef() ||
		namespace.ownership.MaterializerKind() != checkpointmodel.MaterializerNativeTree ||
		!bytes.Equal(reservation.CanonicalBytes(), reservationBytes) {
		return checkpointmodel.ReceiveOperation{}, false, true
	}
	candidate, known := directTreeLookupState(lifecycle)
	if candidate {
		return record, true, false
	}
	if known {
		return checkpointmodel.ReceiveOperation{}, false, false
	}
	return checkpointmodel.ReceiveOperation{}, false, true
}

func directTreeLookupState(lifecycle checkpointmodel.ReceiveLifecycleState) (candidate, known bool) {
	if !lifecycle.Valid() {
		return false, false
	}
	switch lifecycle.Phase() {
	case checkpointmodel.LifecycleIntentFrozen, checkpointmodel.LifecycleReceiving,
		checkpointmodel.LifecycleResumableReceive, checkpointmodel.LifecycleFinalizingTree:
		return true, true
	case checkpointmodel.LifecyclePublished, checkpointmodel.LifecyclePartialDirectory,
		checkpointmodel.LifecycleDiscarded, checkpointmodel.LifecycleExpired,
		checkpointmodel.LifecycleNeedsAttention:
		return false, true
	default:
		return false, false
	}
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

func sameOperationRecord(left, right checkpointmodel.ReceiveOperation) bool {
	leftEncoded, leftErr := checkpointmodel.EncodeReceiveOperation(left)
	rightEncoded, rightErr := checkpointmodel.EncodeReceiveOperation(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftEncoded, rightEncoded)
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
