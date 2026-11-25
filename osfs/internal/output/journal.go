package output

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	journalSyncBatchSize   = 64
	journalSyncInterval    = 2 * time.Second
	journalCompactMinLines = 256
)

var errJournalTailDirty = errors.New("osfs: task journal tail rollback failed")

// journalFile is the narrow file handle contract required by taskJournal.
// *os.File satisfies it directly; tests can wrap it to inject short writes or
// rollback failures without simulating a full filesystem.
type journalFile interface {
	WriteAt(b []byte, off int64) (int, error)
	Truncate(size int64) error
	Sync() error
	Close() error
}

// taskJournal owns append-only persistence and compaction for one operation.
type taskJournal struct {
	mu             sync.Mutex
	opDir          *Dir
	file           journalFile
	offset         int64
	tailDirty      bool
	state          journalState
	dirty          bool
	unflushedCount int
	lastSync       time.Time
	now            func() time.Time
	appendHook     func(kind string) error
}

func createTaskJournal(opDir *Dir, meta taskMeta, now func() time.Time) (*taskJournal, error) {
	if now == nil {
		now = time.Now
	}
	f, err := openChildFile(opDir, journalFileName, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return nil, err
	}
	j := &taskJournal{
		opDir:    opDir,
		file:     f,
		state:    newJournalState(),
		lastSync: now(),
		now:      now,
	}
	rec, err := metaToRecord(meta, true)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := j.appendLocked(rec, true); err != nil {
		_ = f.Close()
		return nil, err
	}
	return j, nil
}

func openTaskJournal(opDir *Dir, now func() time.Time) (*taskJournal, error) {
	if now == nil {
		now = time.Now
	}
	state, validBytes, err := readJournalState(opDir)
	if err != nil {
		return nil, err
	}
	f, err := openChildFile(opDir, journalFileName, os.O_RDWR, privateFileMode)
	if err != nil {
		return nil, err
	}
	if state.tornTail {
		if err := f.Truncate(validBytes); err != nil {
			_ = f.Close()
			return nil, err
		}
		state.tornTail = false
	}
	j := &taskJournal{
		opDir:    opDir,
		file:     f,
		offset:   validBytes,
		state:    state,
		lastSync: now(),
		now:      now,
	}
	if j.shouldCompactLocked() {
		// Compaction is a space optimization; keep the open uncompacted journal
		// when a transient compaction failure leaves j.file valid, matching
		// appendLocked.
		if err := j.compactLocked(); err != nil && j.file == nil {
			return nil, err
		}
	}
	return j, nil
}

func (j *taskJournal) meta() taskMeta {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state.meta
}

func (j *taskJournal) nameMappings() []NameMapping {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]NameMapping, 0, len(j.state.names))
	for _, m := range j.state.names {
		out = append(out, m)
	}
	return out
}

func (j *taskJournal) nameAdjustmentSnapshot() ([]NameMapping, map[string]struct{}, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	hasDelivered := len(j.state.delivered) > 0
	if len(j.state.names) == 0 {
		return nil, nil, hasDelivered
	}
	mappings := make([]NameMapping, 0, len(j.state.names))
	for _, m := range j.state.names {
		mappings = append(mappings, m)
	}
	var delivered map[string]struct{}
	if hasDelivered {
		delivered = make(map[string]struct{}, len(j.state.delivered))
		for path := range j.state.delivered {
			delivered[path] = struct{}{}
		}
	}
	return mappings, delivered, hasDelivered
}

func (j *taskJournal) lookupDelivered(fileID catalog.FileID, path string) (deliveredEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.state.delivered[path]
	if !ok || entry.FileID != fileID {
		return deliveredEntry{}, false
	}
	return entry, true
}

func (j *taskJournal) lookupPartial(path string) (partialEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.state.partials[path]
	if !ok {
		return partialEntry{}, false
	}
	entry.Ranges = slices.Clone(entry.Ranges)
	return entry, true
}

func (j *taskJournal) lookupPublishing(path string) (publishingEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.state.publishing[path]
	return entry, ok
}

func (j *taskJournal) recordLifecycle(lifecycle string, attention AttentionReason) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	next := j.state.meta
	next.Lifecycle = lifecycle
	next.Attention = attention
	next.StateGen++
	rec, err := metaToRecord(next, false)
	if err != nil {
		return err
	}
	return j.appendLocked(rec, true)
}

func (j *taskJournal) recordName(mapping NameMapping) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(nameToRecord(mapping), false)
}

func (j *taskJournal) recordPartial(entry partialEntry, syncNow bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(partialToRecord(entry), syncNow)
}

func (j *taskJournal) recordPublishing(entry publishingEntry, syncNow bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(publishingToRecord(entry), syncNow)
}

func (j *taskJournal) recordDelivered(entry deliveredEntry, syncNow bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(deliveredToRecord(entry), syncNow)
}

func (j *taskJournal) recordRetired(fileID catalog.FileID, path string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, hasPartial := j.state.partials[path]
	_, hasPublishing := j.state.publishing[path]
	if !hasPartial && !hasPublishing {
		return nil
	}
	return j.appendLocked(journalRecord{
		Kind:   "retired",
		FileID: hex.EncodeToString(fileID.Bytes()),
		Path:   path,
	}, false)
}

func (j *taskJournal) flush() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil || !j.dirty {
		return nil
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	j.dirty = false
	j.unflushedCount = 0
	j.lastSync = j.now()
	return nil
}

func (j *taskJournal) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	var err error
	if j.dirty {
		err = j.file.Sync()
		j.dirty = false
		j.unflushedCount = 0
	}
	err = errors.Join(err, j.file.Close())
	j.file = nil
	return err
}

func (j *taskJournal) appendLocked(rec journalRecord, syncNow bool) error {
	if j.file == nil {
		return fs.ErrClosed
	}
	if j.tailDirty {
		return errJournalTailDirty
	}
	if j.appendHook != nil {
		if err := j.appendHook(rec.Kind); err != nil {
			return err
		}
	}
	line, err := encodeJournalLine(rec)
	if err != nil {
		return err
	}
	n, err := j.file.WriteAt(line, j.offset)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return j.rollbackAppendLocked(err)
	}
	if err := j.state.apply(rec); err != nil {
		return j.rollbackAppendLocked(err)
	}
	j.offset += int64(n)
	j.state.lineCount++
	j.dirty = true
	j.unflushedCount++
	if syncNow || j.unflushedCount >= journalSyncBatchSize || j.now().Sub(j.lastSync) >= journalSyncInterval {
		if err := j.file.Sync(); err != nil {
			return err
		}
		j.dirty = false
		j.unflushedCount = 0
		j.lastSync = j.now()
	}
	if j.shouldCompactLocked() {
		// Compaction is a space optimization; the newly appended record is
		// already written (and synced when requested), so a transient
		// compaction failure must not fail the append as long as j.file stays open.
		if err := j.compactLocked(); err != nil && j.file == nil {
			return err
		}
	}
	return nil
}

// rollbackAppendLocked truncates any partial or rejected bytes written beyond
// j.offset so a later append cannot concatenate onto a torn record and trap
// corruption in the middle of state.log. If truncation itself fails, further
// appends are refused so the torn bytes stay strictly at EOF for reopen.
func (j *taskJournal) rollbackAppendLocked(cause error) error {
	if truncErr := j.file.Truncate(j.offset); truncErr != nil {
		j.tailDirty = true
		return errors.Join(cause, truncErr)
	}
	return cause
}

func (j *taskJournal) shouldCompactLocked() bool {
	live := 1 + len(j.state.names) + len(j.state.partials) + len(j.state.publishing) + len(j.state.delivered)
	return j.state.lineCount >= journalCompactMinLines && j.state.lineCount > 2*live
}

func (j *taskJournal) compactLocked() error {
	var buf bytes.Buffer
	lines := 0
	writeRec := func(rec journalRecord) error {
		line, err := encodeJournalLine(rec)
		if err != nil {
			return err
		}
		buf.Write(line)
		lines++
		return nil
	}
	metaRec, err := metaToRecord(j.state.meta, true)
	if err != nil {
		return err
	}
	if err := writeRec(metaRec); err != nil {
		return err
	}
	for _, m := range j.state.names {
		if err := writeRec(nameToRecord(m)); err != nil {
			return err
		}
	}
	for _, p := range j.state.partials {
		if err := writeRec(partialToRecord(p)); err != nil {
			return err
		}
	}
	for _, p := range j.state.publishing {
		if err := writeRec(publishingToRecord(p)); err != nil {
			return err
		}
	}
	for _, d := range j.state.delivered {
		if err := writeRec(deliveredToRecord(d)); err != nil {
			return err
		}
	}

	const tmpName = "state.log.compact"
	_ = removeChild(j.opDir, tmpName, false)
	tmp, err := openChildFile(j.opDir, tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		_ = removeChild(j.opDir, tmpName, false)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = removeChild(j.opDir, tmpName, false)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = removeChild(j.opDir, tmpName, false)
		return err
	}
	// Close old file handle before replacing so Windows permits overwrite rename.
	if err := j.file.Close(); err != nil {
		j.file = nil
		return err
	}
	j.file = nil
	renameErr := renameFileNoReplace(j.opDir, tmpName, j.opDir, journalFileName, true)
	if renameErr != nil {
		_ = removeChild(j.opDir, tmpName, false)
	} else {
		_ = j.opDir.Sync()
	}
	reopened, reopenErr := openChildFile(j.opDir, journalFileName, os.O_RDWR, privateFileMode)
	if reopenErr != nil {
		return errors.Join(renameErr, reopenErr)
	}
	j.file = reopened
	if renameErr != nil {
		return renameErr
	}
	j.offset = int64(buf.Len())
	j.tailDirty = false
	j.state.lineCount = lines
	j.dirty = false
	j.unflushedCount = 0
	j.lastSync = j.now()
	return nil
}

func isValidOpDirName(name string) (receivecontract.OperationID, bool) {
	if len(name) != receivecontract.StableIdentityBytes*2 || strings.ToLower(name) != name {
		return receivecontract.OperationID{}, false
	}
	raw, err := hex.DecodeString(name)
	if err != nil {
		return receivecontract.OperationID{}, false
	}
	id, err := receivecontract.OperationIDFromBytes(raw)
	if err != nil {
		return receivecontract.OperationID{}, false
	}
	return id, true
}

func partFileName(fileID catalog.FileID, locatorDigest transfer.MaterializationLocatorDigest) string {
	return hex.EncodeToString(fileID.Bytes()) + "-" + hex.EncodeToString(locatorDigest[:8]) + ".part"
}

// removeOperationDirectory deletes an operation's scratch directory while
// keeping state.log until every other entry is removed. Deleting the journal
// last ensures a transient lock on a .part file leaves the operation
// identifiable for CLI retry, while inspectOperationDir recognizes an empty
// leftover <opID> directory as cleanup-pending if the final rmdir fails.
func removeOperationDirectory(ctrlDir *Dir, opDirName string) error {
	return removeChildTreeDeferred(ctrlDir, opDirName, journalFileName)
}

func isEmptyDir(dir *Dir) (bool, error) {
	names, err := readDirNames(dir)
	if err != nil {
		return false, err
	}
	return len(names) == 0, nil
}

func removeChildTree(parent *Dir, name string) error {
	return removeChildTreeDeferred(parent, name, "")
}

func removeChildTreeDeferred(parent *Dir, name, deferChild string) error {
	dir, err := openChildDir(parent, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := readDirNames(dir)
	if err != nil {
		return errors.Join(err, dir.Close())
	}
	var removeErr error
	deferredPresent := false
	for _, child := range names {
		if deferChild != "" && child == deferChild {
			deferredPresent = true
			continue
		}
		removeErr = errors.Join(removeErr, removeChildEntry(dir, child))
	}
	if removeErr == nil && deferredPresent {
		removeErr = errors.Join(removeErr, removeChildEntry(dir, deferChild))
	}
	if closeErr := dir.Close(); closeErr != nil {
		removeErr = errors.Join(removeErr, closeErr)
	}
	if removeErr != nil {
		return removeErr
	}
	return removeChild(parent, name, true)
}

func removeChildEntry(dir *Dir, child string) error {
	kind, _, statErr := lstatChild(dir, child)
	if statErr != nil {
		return statErr
	}
	switch kind {
	case EntryAbsent:
		return nil
	case EntryDirectory:
		return removeChildTree(dir, child)
	case EntryOther:
		// On Windows a directory junction reports EntryOther but requires
		// FILE_DIRECTORY_FILE deletion; try non-directory first, then directory.
		if err := removeChild(dir, child, false); err != nil {
			if errors.Is(err, ErrUnsafeNamespace) {
				return removeChild(dir, child, true)
			}
			return err
		}
		return nil
	default:
		return removeChild(dir, child, false)
	}
}
