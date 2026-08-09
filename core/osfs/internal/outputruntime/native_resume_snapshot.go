package outputruntime

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func listNativeResumeSnapshot(
	namespace *checkpointstore.Namespace,
	operations outputcap.Directory,
	candidate checkpointmodel.ReceiveOperation,
	operationBytes []byte,
) (result NativeResumeSnapshot, resultErr error) {
	lease, err := namespace.AcquireOperation(
		candidate.OperationID(), candidate.ReceiveIntentDigest(), candidate.BindingDigest(),
	)
	if err != nil {
		return NativeResumeSnapshot{}, nativeResumeError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, nativeResumeError(lease.Close())) }()
	repository, err := lease.OpenExistingRepository()
	if err != nil {
		if nativeResumeUncertain(err) {
			return uncertainNativeResumeSnapshot(operationBytes), nil
		}
		return NativeResumeSnapshot{}, nativeResumeError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, nativeResumeError(repository.Close())) }()
	stored, operationErr := repository.ReadOperation()
	intent, intentErr := candidate.VerifyIntent(transfer.DecodeReceiveIntent)
	verificationErr := verifyStoredOperation(&repository, intent)
	if operationErr != nil || intentErr != nil || verificationErr != nil ||
		!sameNativeResumeOperation(stored, candidate) {
		joined := errors.Join(operationErr, intentErr, verificationErr, checkpointmodel.ErrRecordBinding)
		if nativeResumeUncertain(joined) {
			return uncertainNativeResumeSnapshot(operationBytes), nil
		}
		return NativeResumeSnapshot{}, nativeResumeError(joined)
	}
	lifecycle, lifecycleErr := repository.ReadLifecycleState()
	if lifecycleErr != nil {
		if !nativeResumeUncertain(lifecycleErr) {
			return NativeResumeSnapshot{}, nativeResumeError(lifecycleErr)
		}
		lifecycleBytes, readErr := readNativeResumeLifecycle(operations, candidate.OperationID())
		if readErr != nil {
			lifecycleBytes = []byte{0}
		}
		return NativeResumeSnapshot{
			OperationRecord: slices.Clone(operationBytes),
			LifecycleRecord: lifecycleBytes,
		}, nil
	}
	encodedLifecycle, err := checkpointmodel.EncodeReceiveLifecycleState(lifecycle)
	if err != nil {
		return NativeResumeSnapshot{}, err
	}
	return NativeResumeSnapshot{
		OperationRecord: slices.Clone(operationBytes),
		LifecycleRecord: encodedLifecycle,
	}, nil
}

func uncertainNativeResumeSnapshot(operation []byte) NativeResumeSnapshot {
	return NativeResumeSnapshot{OperationRecord: slices.Clone(operation), LifecycleRecord: []byte{0}}
}

func sameNativeResumeOperation(left, right checkpointmodel.ReceiveOperation) bool {
	leftBytes, leftErr := checkpointmodel.EncodeReceiveOperation(left)
	rightBytes, rightErr := checkpointmodel.EncodeReceiveOperation(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func nativeResumeOperationNames(operations outputcap.Directory) ([]string, error) {
	if operations == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	names, err := operations.Names(checkpointstore.EntryLimit)
	if err != nil {
		return nil, err
	}
	if len(names) >= checkpointstore.EntryLimit {
		return nil, outputcap.ErrUnsafeNamespace
	}
	slices.Sort(names)
	for _, name := range names {
		if _, err := parseNativeResumeOperationName(name); err != nil {
			return nil, errors.Join(ErrNativeResumeOwnershipUnknown, err)
		}
	}
	return names, nil
}

func readNativeResumeOperation(
	operations outputcap.Directory,
	name string,
) (checkpointmodel.ReceiveOperation, []byte, error) {
	operationID, err := parseNativeResumeOperationName(name)
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, nil, err
	}
	directory, err := openNativeResumeDirectory(operations, name, true)
	if err != nil {
		return checkpointmodel.ReceiveOperation{}, nil, err
	}
	encoded, readErr := checkpointstore.ReadFile(directory, checkpointstore.OperationFile)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return checkpointmodel.ReceiveOperation{}, nil, errors.Join(readErr, closeErr)
	}
	record, err := checkpointmodel.DecodeReceiveOperation(encoded)
	if err != nil || record.OperationID() != operationID {
		return checkpointmodel.ReceiveOperation{}, nil, errors.Join(err, checkpointmodel.ErrRecordBinding)
	}
	canonical, err := checkpointmodel.EncodeReceiveOperation(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return checkpointmodel.ReceiveOperation{}, nil, errors.Join(err, checkpointmodel.ErrRecordNonCanonical)
	}
	return record, canonical, nil
}

func readNativeResumeLifecycle(
	operations outputcap.Directory,
	operation receivecontract.OperationID,
) ([]byte, error) {
	directory, err := openNativeResumeDirectory(operations, hex.EncodeToString(operation.Bytes()), true)
	if err != nil {
		return nil, err
	}
	receipts, err := openNativeResumeDirectory(directory, checkpointstore.ReceiptsDirectory, true)
	if err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	encoded, readErr := checkpointstore.ReadFile(receipts, "lifecycle")
	return encoded, errors.Join(readErr, receipts.Close(), directory.Close())
}

func parseNativeResumeOperationName(name string) (receivecontract.OperationID, error) {
	if len(name) != receivecontract.StableIdentityBytes*2 || name != strings.ToLower(name) {
		return receivecontract.OperationID{}, checkpointmodel.ErrRecordBinding
	}
	raw, err := hex.DecodeString(name)
	if err != nil {
		return receivecontract.OperationID{}, checkpointmodel.ErrRecordBinding
	}
	return receivecontract.OperationIDFromBytes(raw)
}

func openNativeResumeOperations(root outputcap.Directory) (outputcap.Directory, error) {
	control, err := openNativeResumeDirectory(root, checkpointstore.ControlDirectory, true)
	if err != nil {
		return nil, err
	}
	checkpoints, err := openNativeResumeDirectory(control, checkpointstore.CheckpointDirectory, true)
	if err != nil {
		return nil, errors.Join(err, control.Close())
	}
	operations, err := openNativeResumeDirectory(checkpoints, checkpointstore.OperationsDirectory, true)
	closeErr := errors.Join(checkpoints.Close(), control.Close())
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr, closeNativeResumeDirectory(operations))
	}
	return operations, nil
}

func openNativeResumeDirectory(
	parent outputcap.Directory,
	name string,
	private bool,
) (outputcap.Directory, error) {
	if parent == nil || name == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, fs.ErrNotExist
	}
	if !exact || kind != outputcap.EntryDirectory {
		return nil, outputcap.ErrUnsafeNamespace
	}
	directory, err := parent.OpenDirectory(name, private)
	if err != nil || directory == nil {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace, closeNativeResumeDirectory(directory))
	}
	return directory, nil
}

func closeNativeResumeRepository(repository *checkpointstore.Repository) error {
	if repository == nil {
		return nil
	}
	return repository.Close()
}

func closeNativeResumeOperationLease(lease *checkpointstore.OperationLease) error {
	if lease == nil {
		return nil
	}
	return lease.Close()
}

func closeNativeResumeNamespace(namespace *checkpointstore.Namespace) error {
	if namespace == nil {
		return nil
	}
	return namespace.Close()
}

func closeNativeResumePlatform(platform outputcap.Platform) error {
	if platform == nil {
		return nil
	}
	return platform.Close()
}
