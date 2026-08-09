package checkpointstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	fileExecutionLookupDomain = "windshare/file-execution-checkpoint-lookup/v2"
	ownedAnchorSuffix         = ".anchor"
	ownedStageSuffix          = ".stage"
)

// FileExecutionStore is the native adapter for the checkpoint-native file
// engine. Private stage and anchor handles never leave checkpointstore; public
// publication receives only semantic compare/link operations over an object ID.
type FileExecutionStore struct {
	mu sync.Mutex

	repository *Repository
	records    map[[sha256.Size]byte]checkpointmodel.Record
}

var (
	_ fileexecution.CheckpointRepository = (*FileExecutionStore)(nil)
	_ fileexecution.Platform             = (*FileExecutionStore)(nil)
)

func NewFileExecutionStore(repository *Repository) (*FileExecutionStore, error) {
	if repository == nil || repository.records == nil || repository.anchors == nil || repository.stages == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	store := &FileExecutionStore{
		repository: repository,
		records:    make(map[[sha256.Size]byte]checkpointmodel.Record),
	}
	snapshot, err := repository.Reconcile(store.candidateDurable)
	if err != nil {
		return nil, err
	}
	if len(snapshot.Attention()) != 0 {
		return nil, codedError(
			ErrorUnsafeInstall,
			"reconcile file execution checkpoints",
			checkpointmodel.ErrRecordRecovery,
		)
	}
	for _, record := range snapshot.Records() {
		if err := store.indexRecord(record); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// RecordCount reports only the already-reconciled in-memory index. It never
// re-enumerates the namespace, so tracing cannot accidentally perform checkpoint
// I/O outside the intent lease acquired by composition.
func (store *FileExecutionStore) RecordCount() uint64 {
	if store == nil {
		return 0
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return uint64(len(store.records))
}

func (store *FileExecutionStore) Lookup(
	ctx context.Context,
	key fileexecution.CheckpointKey,
) (checkpointmodel.Record, bool, error) {
	if store == nil || ctx == nil {
		return checkpointmodel.Record{}, false, dependencyBoundaryError(
			"lookup file execution checkpoint", transfer.ErrInvalidOutputBinding,
		)
	}
	if err := ctx.Err(); err != nil {
		return checkpointmodel.Record{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record, found := store.records[checkpointLookupKey(
		key.OperationID(),
		key.ReceiveIntentDigest(),
		key.MaterializationBindingDigest(),
		key.FileID().Bytes(),
		key.FileRevision().Bytes(),
		key.CanonicalPath(),
		key.ExactSize(),
		key.MaterializerKind(),
		key.AuthorityRef(),
	)]
	if !found {
		return checkpointmodel.Record{}, false, nil
	}
	if !checkpointKeyMatchesRecord(key, record) {
		return checkpointmodel.Record{}, false, codedError(
			ErrorCorruptRecord,
			"lookup file execution checkpoint",
			checkpointmodel.ErrRecordBinding,
		)
	}
	return record, true, nil
}

func (store *FileExecutionStore) Store(
	ctx context.Context,
	previous *checkpointmodel.Record,
	next checkpointmodel.Record,
) (fileexecution.CheckpointObservation, error) {
	if store == nil || store.repository == nil || ctx == nil || !next.Valid() {
		return fileexecution.CheckpointObservation{}, dependencyBoundaryError(
			"store file execution checkpoint", transfer.ErrInvalidOutputBinding,
		)
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.CheckpointObservation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	var operationErr error
	if previous == nil {
		operationErr = store.repository.Create(next)
	} else {
		operationErr = store.repository.Replace(*previous, next)
	}
	observed, observationErr := store.repository.Reopen(next.RecordID())
	if observationErr != nil {
		if errors.Is(observationErr, fs.ErrNotExist) {
			delete(store.records, checkpointRecordLookupKey(next))
			return fileexecution.MissingCheckpoint(), transferfault.ReduceBoundaryErrors(ctx, operationErr)
		}
		return fileexecution.CheckpointObservation{}, transferfault.ReduceBoundaryErrors(
			ctx, operationErr, observationErr,
		)
	}
	if err := store.indexRecord(observed); err != nil {
		return fileexecution.CheckpointObservation{}, transferfault.ReduceBoundaryErrors(ctx, operationErr, err)
	}
	observation, err := fileexecution.ObservedCheckpoint(observed)
	return observation, transferfault.ReduceBoundaryErrors(ctx, operationErr, err)
}

func (store *FileExecutionStore) indexRecord(record checkpointmodel.Record) error {
	if !record.Valid() {
		return codedError(ErrorCorruptRecord, "index file execution checkpoint", checkpointmodel.ErrInvalidRecord)
	}
	key := checkpointRecordLookupKey(record)
	if current, found := store.records[key]; found && current.RecordID() != record.RecordID() {
		return codedError(ErrorCorruptRecord, "index file execution checkpoint", checkpointmodel.ErrRecordBinding)
	}
	store.records[key] = record
	return nil
}

func checkpointRecordLookupKey(record checkpointmodel.Record) [sha256.Size]byte {
	return checkpointLookupKey(
		record.OperationID(),
		record.ReceiveIntentDigest(),
		record.MaterializationBindingDigest(),
		record.FileID().Bytes(),
		record.FileRevision().Bytes(),
		record.CanonicalPath(),
		record.ExactSize(),
		record.MaterializerKind(),
		record.AuthorityRef(),
	)
}

func checkpointLookupKey(
	operation receivecontract.OperationID,
	intent transfer.ReceiveIntentDigest,
	materialization receivecontract.BindingDigest,
	fileID []byte,
	revision []byte,
	path string,
	exactSize uint64,
	materializer checkpointmodel.MaterializerKind,
	authority receivecontract.AuthorityRef,
) [sha256.Size]byte {
	hash := sha256.New()
	writeExecutionLookupBytes(hash, []byte(fileExecutionLookupDomain))
	writeExecutionLookupBytes(hash, operation.Bytes())
	writeExecutionLookupBytes(hash, intent.Bytes())
	writeExecutionLookupBytes(hash, materialization.Bytes())
	writeExecutionLookupBytes(hash, fileID)
	writeExecutionLookupBytes(hash, revision)
	writeExecutionLookupBytes(hash, []byte(path))
	var encodedSize [8]byte
	binary.BigEndian.PutUint64(encodedSize[:], exactSize)
	_, _ = hash.Write(encodedSize[:])
	writeExecutionLookupBytes(hash, []byte{byte(materializer)})
	writeExecutionLookupBytes(hash, authority.Bytes())
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func writeExecutionLookupBytes(target io.Writer, value []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = target.Write(size[:])
	_, _ = target.Write(value)
}

func checkpointKeyMatchesRecord(key fileexecution.CheckpointKey, record checkpointmodel.Record) bool {
	return record.Valid() && record.OperationID() == key.OperationID() &&
		record.ReceiveIntentDigest() == key.ReceiveIntentDigest() &&
		record.MaterializationBindingDigest() == key.MaterializationBindingDigest() &&
		record.FileID() == key.FileID() && record.FileRevision() == key.FileRevision() &&
		record.CanonicalPath() == key.CanonicalPath() && record.ExactSize() == key.ExactSize() &&
		record.MaterializerKind() == key.MaterializerKind() && record.AuthorityRef() == key.AuthorityRef()
}
