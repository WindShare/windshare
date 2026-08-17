package checkpointstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"slices"
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
	profile    checkpointmodel.LiveCleanupNativeProfile
	records    map[[sha256.Size]byte]checkpointmodel.Record
	retained   map[checkpointmodel.RecordID]checkpointmodel.Record
	attention  []Attention
}

var (
	_ fileexecution.CheckpointRepository = (*FileExecutionStore)(nil)
	_ fileexecution.Platform             = (*FileExecutionStore)(nil)
)

func NewFileExecutionStore(repository *Repository) (*FileExecutionStore, error) {
	return NewFileExecutionStoreWithProfile(repository, 0)
}

func NewFileExecutionStoreWithProfile(
	repository *Repository,
	profile checkpointmodel.LiveCleanupNativeProfile,
) (*FileExecutionStore, error) {
	return newFileExecutionStore(repository, profile, true)
}

// NewFreshFileExecutionStore deliberately skips repository enumeration. It is
// valid only for a newly published ordinary operation whose private namespace
// cannot contain an earlier file record.
func NewFreshFileExecutionStore(repository *Repository) (*FileExecutionStore, error) {
	return NewFreshFileExecutionStoreWithProfile(repository, 0)
}

func NewFreshFileExecutionStoreWithProfile(
	repository *Repository,
	profile checkpointmodel.LiveCleanupNativeProfile,
) (*FileExecutionStore, error) {
	return newFileExecutionStore(repository, profile, false)
}

func newFileExecutionStore(
	repository *Repository,
	profile checkpointmodel.LiveCleanupNativeProfile,
	reconcile bool,
) (*FileExecutionStore, error) {
	if repository == nil || repository.records == nil || repository.anchors == nil || repository.stages == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	store := &FileExecutionStore{
		repository: repository,
		profile:    profile,
		records:    make(map[[sha256.Size]byte]checkpointmodel.Record),
		retained:   make(map[checkpointmodel.RecordID]checkpointmodel.Record),
	}
	if !reconcile {
		return store, nil
	}
	snapshot, err := repository.Reconcile(store.candidateDurable)
	if err != nil {
		return nil, err
	}
	// Corrupt or foreign checkpoint images are inert evidence. They cannot grant
	// range authority, but they also must not make an unrelated file or sibling
	// unresumable; list/discard surface the bounded attention references instead.
	store.attention = snapshot.Attention()
	for _, record := range snapshot.Records() {
		if err := store.indexReconciledRecord(record); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (store *FileExecutionStore) Snapshot() (records []checkpointmodel.Record, attention []Attention) {
	if store == nil {
		return nil, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	records = make([]checkpointmodel.Record, 0, len(store.records))
	for _, record := range store.records {
		records = append(records, record)
	}
	slices.SortFunc(records, func(left, right checkpointmodel.Record) int {
		return bytes.Compare(left.RecordID().Bytes(), right.RecordID().Bytes())
	})
	return records, slices.Clone(store.attention)
}

// CleanupOwned retires only objects whose exact canonical records were already
// authenticated during this operation-scoped reconciliation. Unknown entries
// remain untouched and keep terminal cleanup pending.
func (store *FileExecutionStore) CleanupOwned(ctx context.Context) error {
	if store == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.attention) != 0 {
		return codedError(ErrorUnsafeInstall, "cleanup ordinary file state", checkpointmodel.ErrRecordRecovery)
	}
	records := make([]checkpointmodel.Record, 0, len(store.records)+len(store.retained))
	for _, record := range store.records {
		records = append(records, record)
	}
	for _, record := range store.retained {
		records = append(records, record)
	}
	slices.SortFunc(records, func(left, right checkpointmodel.Record) int {
		return bytes.Compare(left.RecordID().Bytes(), right.RecordID().Bytes())
	})
	for _, record := range records {
		for _, step := range []fileexecution.RetirementStep{
			fileexecution.RetirementRemoveStage,
			fileexecution.RetirementSyncStageNamespace,
			fileexecution.RetirementRemoveAnchor,
			fileexecution.RetirementSyncAnchorNamespace,
		} {
			observation, err := store.applyRetirementLocked(record.OwnedObjectID(), step)
			if err != nil || !observation.ValidForCleanup(record.OwnedObjectID()) {
				return errors.Join(err, checkpointmodel.ErrRecordRecovery)
			}
		}
		if err := store.repository.Remove(record); err != nil {
			return err
		}
		delete(store.records, checkpointRecordLookupKey(record))
		delete(store.retained, record.RecordID())
	}
	return nil
}

// RecordCount reports only the selected in-memory index. It never re-enumerates
// the namespace, so tracing cannot accidentally perform checkpoint I/O outside
// the intent lease acquired by composition.
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

func (store *FileExecutionStore) Abandon(
	ctx context.Context,
	record checkpointmodel.Record,
) error {
	if store == nil || ctx == nil || !record.Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	key := checkpointRecordLookupKey(record)
	current, found := store.records[key]
	if !found || current.RecordID() != record.RecordID() {
		return codedError(ErrorCorruptRecord, "abandon file checkpoint lookup", checkpointmodel.ErrRecordBinding)
	}
	delete(store.records, key)
	store.retained[current.RecordID()] = current
	return nil
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

func (store *FileExecutionStore) indexReconciledRecord(record checkpointmodel.Record) error {
	if !record.Valid() {
		return codedError(ErrorCorruptRecord, "index reconciled file checkpoint", checkpointmodel.ErrInvalidRecord)
	}
	key := checkpointRecordLookupKey(record)
	current, found := store.records[key]
	if !found || current.RecordID() == record.RecordID() {
		store.records[key] = record
		return nil
	}
	currentReady, currentErr := store.recordOwnedReady(current)
	nextReady, nextErr := store.recordOwnedReady(record)
	if currentErr != nil || nextErr != nil {
		return errors.Join(currentErr, nextErr)
	}
	switch {
	case currentReady && !nextReady:
		store.retained[record.RecordID()] = record
		return nil
	case !currentReady && nextReady:
		store.retained[current.RecordID()] = current
		store.records[key] = record
		return nil
	default:
		return codedError(ErrorCorruptRecord, "select reconciled file checkpoint", checkpointmodel.ErrRecordBinding)
	}
}

func (store *FileExecutionStore) recordOwnedReady(record checkpointmodel.Record) (bool, error) {
	files, observation, err := store.openObservedOwnedLocked(
		record.OwnedObjectID(), record.ExactSize(), true,
	)
	if err != nil {
		return false, errors.Join(err, files.close())
	}
	return observation.Condition() == fileexecution.OwnedReady, files.close()
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
