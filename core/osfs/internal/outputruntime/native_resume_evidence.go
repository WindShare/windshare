package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const (
	nativeResumeCleanupEvidenceDomain = "windshare/native-resume-cleanup/v1"
	nativeResumeExpiryEvidenceDomain  = "windshare/native-resume-expiry/v1"
)

func (lease *NativeResumeLease) terminalReceiptLocked() ([]byte, error) {
	receipts, err := openNativeResumeDirectory(
		lease.operationDirectory, checkpointstore.ReceiptsDirectory, true,
	)
	if err != nil {
		return nil, err
	}
	defer receipts.Close()
	names, err := receipts.Names(checkpointstore.EntryLimit)
	if err != nil || len(names) >= checkpointstore.EntryLimit {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	slices.Sort(names)
	var terminal checkpointmodel.DirectTreeReceipt
	for _, name := range names {
		if name == "lifecycle" {
			continue
		}
		if len(name) != sha256.Size*2 || name != strings.ToLower(name) {
			return nil, outputcap.ErrUnsafeNamespace
		}
		rawDigest, decodeNameErr := hex.DecodeString(name)
		digest, digestErr := checkpointmodel.AggregateDigestFromBytes(rawDigest)
		encoded, readErr := checkpointstore.ReadFile(receipts, name)
		decoded, decodeErr := checkpointmodel.DecodeDirectTreeReceipt(encoded)
		stored, storedErr := lease.repository.ReadReceipt(digest)
		if decodeNameErr != nil || digestErr != nil || readErr != nil || decodeErr != nil || storedErr != nil ||
			decoded.Digest() != digest || !bytes.Equal(decoded.CanonicalBytes(), stored.CanonicalBytes()) {
			return nil, errors.Join(
				decodeNameErr, digestErr, readErr, decodeErr, storedErr, checkpointmodel.ErrInvalidReceipt,
			)
		}
		if decoded.Kind() != checkpointmodel.ReceiptTreeCompletion &&
			decoded.Kind() != checkpointmodel.ReceiptPartialDirectory {
			continue
		}
		if terminal.Valid() {
			return nil, ErrNativeResumeOwnershipUnknown
		}
		terminal = decoded
	}
	return terminal.CanonicalBytes(), nil
}

func nativeResumeExpiryReceipt(
	operation checkpointmodel.ReceiveOperation,
	lifecycle checkpointmodel.ReceiveLifecycleState,
) ([]byte, error) {
	if lifecycle.Phase() != checkpointmodel.LifecycleResumableReceive {
		return nil, nil
	}
	if lifecycle.StateGeneration() == ^uint64(0) {
		return nil, checkpointmodel.ErrInvalidLifecycleState
	}
	evidence, err := nativeResumeEvidenceDigest(
		nativeResumeExpiryEvidenceDomain,
		operation,
		lifecycle,
		lifecycle.CheckpointReferences(),
		nil,
	)
	if err != nil {
		return nil, err
	}
	receipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind:        checkpointmodel.ReceiptExpiry,
		OperationID: operation.OperationID(), ReceiveIntent: operation.ReceiveIntentDigest(),
		ReservationDigest: operation.BindingDigest(), CheckpointRefs: lifecycle.CheckpointReferences(),
		EvidenceDigest: evidence, SuccessCount: lifecycle.SuccessCount(), FailureCount: lifecycle.FailureCount(),
		CleanupGeneration: lifecycle.StateGeneration() + 1,
	})
	if err != nil {
		return nil, err
	}
	return receipt.CanonicalBytes(), nil
}

func nativeResumeCleanupReceipt(
	operation checkpointmodel.ReceiveOperation,
	lifecycle checkpointmodel.ReceiveLifecycleState,
	records []checkpointmodel.Record,
	objects []checkpointmodel.ObjectID,
) (checkpointmodel.DirectTreeReceipt, error) {
	if lifecycle.StateGeneration() == ^uint64(0) {
		return checkpointmodel.DirectTreeReceipt{}, checkpointmodel.ErrInvalidLifecycleState
	}
	references := make([]checkpointmodel.FileCheckpointReference, 0, len(records))
	for _, record := range records {
		reference, err := checkpointmodel.NewFileCheckpointReference(record)
		if err != nil {
			return checkpointmodel.DirectTreeReceipt{}, err
		}
		references = append(references, reference)
	}
	evidence, err := nativeResumeEvidenceDigest(
		nativeResumeCleanupEvidenceDomain, operation, lifecycle, references, objects,
	)
	if err != nil {
		return checkpointmodel.DirectTreeReceipt{}, err
	}
	return checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind:        checkpointmodel.ReceiptCleanup,
		OperationID: operation.OperationID(), ReceiveIntent: operation.ReceiveIntentDigest(),
		ReservationDigest: operation.BindingDigest(), EvidenceDigest: evidence,
		CleanupGeneration:  lifecycle.StateGeneration() + 1,
		RemovedObjectCount: uint64(len(objects)), RemovedRecordCount: 0,
	})
}

func nativeResumeEvidenceDigest(
	domain string,
	operation checkpointmodel.ReceiveOperation,
	lifecycle checkpointmodel.ReceiveLifecycleState,
	references []checkpointmodel.FileCheckpointReference,
	objects []checkpointmodel.ObjectID,
) (checkpointmodel.AggregateDigest, error) {
	if domain == "" || !operation.Valid() || !lifecycle.Valid() {
		return checkpointmodel.AggregateDigest{}, transfer.ErrInvalidOutputBinding
	}
	hash := sha256.New()
	writeNativeResumeEvidenceField(hash, []byte(domain))
	writeNativeResumeEvidenceField(hash, operation.OperationID().Bytes())
	writeNativeResumeEvidenceField(hash, operation.ReceiveIntentDigest().Bytes())
	writeNativeResumeEvidenceField(hash, operation.BindingDigest().Bytes())
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], lifecycle.StateGeneration())
	_, _ = hash.Write(generation[:])
	canonicalReferences := slices.Clone(references)
	slices.SortFunc(canonicalReferences, func(left, right checkpointmodel.FileCheckpointReference) int {
		return bytes.Compare(left.RecordID().Bytes(), right.RecordID().Bytes())
	})
	for _, reference := range canonicalReferences {
		writeNativeResumeEvidenceField(hash, reference.RecordID().Bytes())
		binary.BigEndian.PutUint64(generation[:], reference.CheckpointGeneration())
		_, _ = hash.Write(generation[:])
	}
	canonicalObjects := slices.Clone(objects)
	slices.SortFunc(canonicalObjects, func(left, right checkpointmodel.ObjectID) int {
		return bytes.Compare(left.Bytes(), right.Bytes())
	})
	for _, object := range canonicalObjects {
		writeNativeResumeEvidenceField(hash, object.Bytes())
	}
	return checkpointmodel.AggregateDigestFromBytes(hash.Sum(nil))
}

func writeNativeResumeEvidenceField(hash interface{ Write([]byte) (int, error) }, value []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write(value)
}

func nativeResumeObjects(records []checkpointmodel.Record) []checkpointmodel.ObjectID {
	seen := make(map[checkpointmodel.ObjectID]struct{}, len(records))
	result := make([]checkpointmodel.ObjectID, 0, len(records))
	for _, record := range records {
		if _, exists := seen[record.OwnedObjectID()]; exists {
			continue
		}
		seen[record.OwnedObjectID()] = struct{}{}
		result = append(result, record.OwnedObjectID())
	}
	slices.SortFunc(result, func(left, right checkpointmodel.ObjectID) int {
		return bytes.Compare(left.Bytes(), right.Bytes())
	})
	return result
}

func unknownNativeResumeEvidence(
	lifecycle checkpointmodel.ReceiveLifecycleState,
) NativeResumeRecoveryEvidence {
	cleanup := NativeResumeCleanupUnknown
	if lifecycle.Valid() && lifecycle.CleanupState() == checkpointmodel.OwnedCleanupClean {
		cleanup = NativeResumeCleanupComplete
	}
	return NativeResumeRecoveryEvidence{
		TargetOwnership: NativeResumeEvidenceUnknown,
		Checkpoints:     NativeResumeEvidenceUnknown,
		Cleanup:         cleanup,
	}
}

func nativeResumeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrNativeResumeBusy) {
		return err
	}
	var checkpointErr *checkpointstore.Error
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
		errors.As(err, &checkpointErr) && checkpointErr != nil &&
			checkpointErr.Code() == checkpointstore.ErrorBusy {
		return errors.Join(ErrNativeResumeBusy, err)
	}
	return err
}

func nativeResumeUncertain(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrNativeResumeBusy) {
		return false
	}
	if errors.Is(err, ErrNativeResumeOwnershipUnknown) || errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, outputcap.ErrUnsafeNamespace) || errors.Is(err, outputcap.ErrNamespaceCollision) ||
		errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) ||
		errors.Is(err, directoryauthority.ErrRetainedAuthorityChanged) ||
		errors.Is(err, checkpointmodel.ErrInvalidOwnership) ||
		errors.Is(err, checkpointmodel.ErrOwnershipChecksum) ||
		errors.Is(err, checkpointmodel.ErrOwnershipNonCanonical) ||
		errors.Is(err, checkpointmodel.ErrInvalidAdmittedDirectory) ||
		errors.Is(err, checkpointmodel.ErrInvalidRecord) ||
		errors.Is(err, checkpointmodel.ErrRecordChecksum) ||
		errors.Is(err, checkpointmodel.ErrRecordNonCanonical) ||
		errors.Is(err, checkpointmodel.ErrRecordBinding) ||
		errors.Is(err, checkpointmodel.ErrRecordGeneration) ||
		errors.Is(err, checkpointmodel.ErrRecordObjectConflict) ||
		errors.Is(err, checkpointmodel.ErrRecordRecovery) ||
		errors.Is(err, checkpointmodel.ErrRecordCrashBoundary) ||
		errors.Is(err, checkpointmodel.ErrInvalidReceipt) ||
		errors.Is(err, checkpointmodel.ErrInvalidLifecycleState) {
		return true
	}
	var checkpointErr *checkpointstore.Error
	if !errors.As(err, &checkpointErr) || checkpointErr == nil {
		return false
	}
	switch checkpointErr.Code() {
	case checkpointstore.ErrorCorruptRecord,
		checkpointstore.ErrorUnsafeInstall,
		checkpointstore.ErrorOwnershipMismatch:
		return true
	default:
		return false
	}
}
