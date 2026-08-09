package checkpointstore

import (
	"bytes"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const (
	recordSuffix      = ".checkpoint"
	recordIDHexLength = 64
	recordShardLength = 2
	attentionDomain   = "windshare/checkpoint-attention/v2"
)

// Repository owns one leased operation's immutable operation image, aggregate
// records, and FileCheckpointV2 records. Aggregate records never carry ranges.
type Repository struct {
	operation   outputcap.Directory
	checkpoints outputcap.Directory
	records     outputcap.Directory
	anchors     outputcap.Directory
	stages      outputcap.Directory
	manifests   outputcap.Directory
	receipts    outputcap.Directory
	binding     checkpointmodel.Binding
}

func (repository *Repository) Close() error {
	if repository == nil {
		return nil
	}
	err := errors.Join(
		closeDirectory(repository.records),
		closeDirectory(repository.anchors),
		closeDirectory(repository.stages),
		closeDirectory(repository.manifests),
		closeDirectory(repository.receipts),
		closeDirectory(repository.checkpoints),
		closeDirectory(repository.operation),
	)
	*repository = Repository{}
	return repositoryError("close operation repository", err)
}

func (repository *Repository) Create(record checkpointmodel.Record) error {
	if err := repository.validateRecord(record); err != nil {
		return err
	}
	encoded, err := checkpointmodel.EncodeRecord(record)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode created record", err)
	}
	if !checkpointmodel.InitialCandidate(record) {
		// Only the empty generation-zero candidate can establish a new file
		// authority. Later cuts need a predecessor transition; an exact existing
		// image is accepted solely to make a completed create retry idempotent.
		existing, reopenErr := repository.Reopen(record.RecordID())
		if reopenErr != nil {
			return codedError(
				ErrorUnsafeInstall, "create checkpoint without predecessor",
				errors.Join(checkpointmodel.ErrRecordGeneration, reopenErr),
			)
		}
		existingEncoded, encodeErr := checkpointmodel.EncodeRecord(existing)
		if encodeErr != nil || !bytes.Equal(existingEncoded, encoded) {
			return codedError(
				ErrorUnsafeInstall, "create checkpoint without predecessor",
				errors.Join(checkpointmodel.ErrRecordGeneration, encodeErr),
			)
		}
		return nil
	}
	shardName, recordName := recordLocation(record.RecordID())
	shard, err := OpenShard(repository.records, shardName, true)
	if err != nil {
		return repositoryError("open created record shard", err)
	}
	installErr := InstallCreate(shard, recordName, encoded)
	return repositoryError("create checkpoint record", errors.Join(installErr, shard.Close()))
}

func (repository *Repository) Replace(previous, next checkpointmodel.Record) error {
	if err := repository.validateRecord(previous); err != nil {
		return err
	}
	if err := repository.validateRecord(next); err != nil {
		return err
	}
	if previous.RecordID() != next.RecordID() {
		return codedError(ErrorUnsafeInstall, "replace checkpoint record", checkpointmodel.ErrRecordBinding)
	}
	if err := checkpointmodel.ValidateTransition(previous, next); err != nil {
		return codedError(ErrorUnsafeInstall, "validate checkpoint replacement", err)
	}
	previousEncoded, err := checkpointmodel.EncodeRecord(previous)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode previous record", err)
	}
	nextEncoded, err := checkpointmodel.EncodeRecord(next)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode next record", err)
	}
	shardName, recordName := recordLocation(next.RecordID())
	shard, err := OpenShard(repository.records, shardName, false)
	if err != nil {
		return repositoryError("open replacement record shard", err)
	}
	installErr := InstallReplace(shard, recordName, previousEncoded, nextEncoded)
	return repositoryError("replace checkpoint record", errors.Join(installErr, shard.Close()))
}

func (repository *Repository) Reopen(recordID checkpointmodel.RecordID) (checkpointmodel.Record, error) {
	if repository == nil || repository.records == nil || recordID.IsZero() {
		return checkpointmodel.Record{}, transfer.ErrInvalidOutputBinding
	}
	shardName, recordName := recordLocation(recordID)
	shard, err := OpenShard(repository.records, shardName, false)
	if err != nil {
		return checkpointmodel.Record{}, repositoryError("open record shard", err)
	}
	encoded, readErr := ReadFile(shard, recordName)
	closeErr := shard.Close()
	if readErr != nil || closeErr != nil {
		return checkpointmodel.Record{}, repositoryError("read checkpoint record", errors.Join(readErr, closeErr))
	}
	record, err := checkpointmodel.DecodeRecord(encoded)
	if err != nil {
		return checkpointmodel.Record{}, codedError(ErrorCorruptRecord, "decode checkpoint record", err)
	}
	if !repository.binding.Matches(record, recordID) {
		return checkpointmodel.Record{}, codedError(ErrorCorruptRecord, "bind checkpoint record", checkpointmodel.ErrRecordBinding)
	}
	return record, nil
}

func (repository *Repository) Remove(record checkpointmodel.Record) error {
	if err := repository.validateRecord(record); err != nil {
		return err
	}
	encoded, err := checkpointmodel.EncodeRecord(record)
	if err != nil {
		return codedError(ErrorCorruptRecord, "encode removed record", err)
	}
	shardName, recordName := recordLocation(record.RecordID())
	shard, err := OpenShard(repository.records, shardName, false)
	if err != nil {
		return repositoryError("open removed record shard", err)
	}
	operationErr, cleanupErr := RemoveExact(shard, recordName, encoded)
	return repositoryError("remove checkpoint record", errors.Join(operationErr, cleanupErr, shard.Close()))
}

func (repository *Repository) validateRecord(record checkpointmodel.Record) error {
	if repository == nil || repository.records == nil || record.RecordID().IsZero() {
		return transfer.ErrInvalidOutputBinding
	}
	if !repository.binding.Matches(record, record.RecordID()) {
		return codedError(ErrorCorruptRecord, "bind checkpoint record", checkpointmodel.ErrRecordBinding)
	}
	return nil
}
