package checkpointstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

const lifecycleStateFile = "lifecycle"

const admittedDirectoryPrefix = "directory-"

func (repository *Repository) InstallAdmittedDirectory(record checkpointmodel.AdmittedDirectory) error {
	if repository == nil || repository.manifests == nil || !record.Valid() ||
		record.OperationID() != repository.binding.OperationID() ||
		record.ReceiveIntentDigest() != repository.binding.ReceiveIntentDigest() {
		return codedError(ErrorCorruptRecord, "bind admitted directory", checkpointmodel.ErrRecordBinding)
	}
	encoded, err := checkpointmodel.EncodeAdmittedDirectory(record)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode admitted directory", err)
	}
	name := admittedDirectoryPrefix + hex.EncodeToString(record.DirectoryID().Bytes())
	return repositoryError("install admitted directory", InstallCreate(repository.manifests, name, encoded))
}

func (repository *Repository) ReadAdmittedDirectory(
	directoryID catalog.DirectoryID,
) (checkpointmodel.AdmittedDirectory, error) {
	if repository == nil || repository.manifests == nil || directoryID.IsZero() {
		return checkpointmodel.AdmittedDirectory{}, transfer.ErrInvalidOutputBinding
	}
	name := admittedDirectoryPrefix + hex.EncodeToString(directoryID.Bytes())
	encoded, err := ReadFile(repository.manifests, name)
	if err != nil {
		return checkpointmodel.AdmittedDirectory{}, repositoryError("read admitted directory", err)
	}
	record, err := checkpointmodel.DecodeAdmittedDirectory(encoded)
	if err != nil || record.DirectoryID() != directoryID ||
		record.OperationID() != repository.binding.OperationID() ||
		record.ReceiveIntentDigest() != repository.binding.ReceiveIntentDigest() {
		return checkpointmodel.AdmittedDirectory{}, codedError(
			ErrorCorruptRecord, "bind admitted directory", errors.Join(err, checkpointmodel.ErrRecordBinding),
		)
	}
	return record, nil
}

func (repository *Repository) InstallReceipt(receipt checkpointmodel.DirectTreeReceipt) error {
	if err := repository.validateReceipt(receipt); err != nil {
		return err
	}
	if err := repository.verifyCheckpointReferences(receipt.CheckpointReferences()); err != nil {
		return err
	}
	encoded := receipt.CanonicalBytes()
	if len(encoded) == 0 || len(encoded) > maxRepositoryRecordBytes {
		return codedError(ErrorCorruptRecord, "encode operation receipt", checkpointmodel.ErrInvalidReceipt)
	}
	name := hex.EncodeToString(receipt.Digest().Bytes())
	return repositoryError("install operation receipt", InstallCreate(repository.receipts, name, encoded))
}

func (repository *Repository) ReadReceipt(
	digest checkpointmodel.AggregateDigest,
) (checkpointmodel.DirectTreeReceipt, error) {
	if repository == nil || repository.receipts == nil || digest.IsZero() {
		return checkpointmodel.DirectTreeReceipt{}, transfer.ErrInvalidOutputBinding
	}
	encoded, err := ReadFile(repository.receipts, hex.EncodeToString(digest.Bytes()))
	if err != nil {
		return checkpointmodel.DirectTreeReceipt{}, repositoryError("read operation receipt", err)
	}
	receipt, err := checkpointmodel.DecodeDirectTreeReceipt(encoded)
	if err != nil || receipt.Digest() != digest {
		return checkpointmodel.DirectTreeReceipt{}, codedError(
			ErrorCorruptRecord, "decode operation receipt", errors.Join(err, checkpointmodel.ErrInvalidReceipt),
		)
	}
	if err := repository.validateReceipt(receipt); err != nil {
		return checkpointmodel.DirectTreeReceipt{}, err
	}
	if err := repository.verifyCheckpointReferences(receipt.CheckpointReferences()); err != nil {
		return checkpointmodel.DirectTreeReceipt{}, err
	}
	return receipt, nil
}

func (repository *Repository) CreateLifecycleState(next checkpointmodel.ReceiveLifecycleState) error {
	if err := repository.validateLifecycle(next); err != nil {
		return err
	}
	if next.StateGeneration() != 1 || next.Phase() != checkpointmodel.LifecycleIntentFrozen {
		return codedError(ErrorUnsafeInstall, "create lifecycle state", checkpointmodel.ErrInvalidLifecycleState)
	}
	if err := repository.verifyLifecycleAuthorities(next); err != nil {
		return err
	}
	encoded, err := checkpointmodel.EncodeReceiveLifecycleState(next)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode lifecycle state", err)
	}
	return repositoryError("create lifecycle state", InstallCreate(repository.receipts, lifecycleStateFile, encoded))
}

func (repository *Repository) ReplaceLifecycleState(
	previous checkpointmodel.ReceiveLifecycleState,
	next checkpointmodel.ReceiveLifecycleState,
) error {
	if err := repository.validateLifecycle(previous); err != nil {
		return err
	}
	if err := repository.validateLifecycle(next); err != nil {
		return err
	}
	if err := checkpointmodel.ValidateLifecycleTransition(previous, next); err != nil {
		return codedError(ErrorUnsafeInstall, "validate lifecycle transition", err)
	}
	if err := repository.verifyLifecycleAuthorities(next); err != nil {
		return err
	}
	previousEncoded, err := checkpointmodel.EncodeReceiveLifecycleState(previous)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode previous lifecycle state", err)
	}
	nextEncoded, err := checkpointmodel.EncodeReceiveLifecycleState(next)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode next lifecycle state", err)
	}
	return repositoryError(
		"replace lifecycle state",
		InstallReplace(repository.receipts, lifecycleStateFile, previousEncoded, nextEncoded),
	)
}

func (repository *Repository) ReadLifecycleState() (checkpointmodel.ReceiveLifecycleState, error) {
	if repository == nil || repository.receipts == nil {
		return checkpointmodel.ReceiveLifecycleState{}, transfer.ErrInvalidOutputBinding
	}
	encoded, err := ReadFile(repository.receipts, lifecycleStateFile)
	if err != nil {
		return checkpointmodel.ReceiveLifecycleState{}, repositoryError("read lifecycle state", err)
	}
	record, err := checkpointmodel.DecodeReceiveLifecycleState(encoded)
	if err != nil {
		return checkpointmodel.ReceiveLifecycleState{}, codedError(ErrorCorruptRecord, "decode lifecycle state", err)
	}
	if err := repository.validateLifecycle(record); err != nil {
		return checkpointmodel.ReceiveLifecycleState{}, err
	}
	if err := repository.verifyLifecycleAuthorities(record); err != nil {
		return checkpointmodel.ReceiveLifecycleState{}, err
	}
	return record, nil
}

func (repository *Repository) validateReceipt(receipt checkpointmodel.DirectTreeReceipt) error {
	if repository == nil || repository.receipts == nil || !receipt.Valid() ||
		receipt.OperationID() != repository.binding.OperationID() ||
		receipt.ReceiveIntentDigest() != repository.binding.ReceiveIntentDigest() ||
		receipt.ReservationDigest() != repository.binding.MaterializationBindingDigest() {
		return codedError(ErrorCorruptRecord, "bind operation receipt", checkpointmodel.ErrRecordBinding)
	}
	return nil
}

func (repository *Repository) validateLifecycle(record checkpointmodel.ReceiveLifecycleState) error {
	if repository == nil || repository.receipts == nil || !record.Valid() ||
		record.OperationID() != repository.binding.OperationID() ||
		record.ReceiveIntentDigest() != repository.binding.ReceiveIntentDigest() {
		return codedError(ErrorCorruptRecord, "bind lifecycle state", checkpointmodel.ErrRecordBinding)
	}
	return nil
}

func (repository *Repository) verifyLifecycleAuthorities(
	record checkpointmodel.ReceiveLifecycleState,
) error {
	if err := repository.verifyCheckpointReferences(record.CheckpointReferences()); err != nil {
		return err
	}
	if record.ReceiptDigest().IsZero() {
		return nil
	}
	_, err := repository.ReadReceipt(record.ReceiptDigest())
	if errors.Is(err, fs.ErrNotExist) {
		return codedError(ErrorUnsafeInstall, "bind lifecycle receipt", checkpointmodel.ErrRecordBinding)
	}
	return err
}

func (repository *Repository) verifyCheckpointReferences(
	references []checkpointmodel.FileCheckpointReference,
) error {
	for _, reference := range references {
		record, err := repository.Reopen(reference.RecordID())
		if err != nil {
			return codedError(
				ErrorCorruptRecord, "bind aggregate checkpoint reference",
				errors.Join(checkpointmodel.ErrRecordBinding, err),
			)
		}
		if record.CheckpointGeneration() != reference.CheckpointGeneration() ||
			(record.CommitState() != checkpointmodel.CommitVerified &&
				record.CommitState() != checkpointmodel.CommitPublished) {
			return codedError(
				ErrorCorruptRecord, "bind aggregate checkpoint generation", checkpointmodel.ErrRecordBinding,
			)
		}
	}
	return nil
}

func sameLifecycleState(left, right checkpointmodel.ReceiveLifecycleState) bool {
	leftEncoded, leftErr := checkpointmodel.EncodeReceiveLifecycleState(left)
	rightEncoded, rightErr := checkpointmodel.EncodeReceiveLifecycleState(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftEncoded, rightEncoded)
}
