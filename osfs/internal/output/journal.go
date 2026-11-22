package output

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	journalSyncBatchSize    = 64
	journalSyncInterval     = 2 * time.Second
	journalCompactMinLines  = 256
	journalMaxLineBytes     = 4 << 20
	journalLifecycleActive  = "active"
	journalLifecycleStopped = "share_stopped"
	journalLifecycleAttn    = "attention"
)

var errJournalCorrupt = errors.New("osfs: task journal is corrupt")

type journalRecord struct {
	Kind string `json:"k"`

	// Meta record ("meta")
	Intent          []byte `json:"intent,omitempty"`
	SelectionDigest string `json:"sel,omitempty"`
	Redownload      bool   `json:"redl,omitempty"`
	Lifecycle       string `json:"life,omitempty"`
	Attention       string `json:"attn,omitempty"`
	StateGen        uint64 `json:"sgen,omitempty"`

	// Name mapping record ("name")
	LogicalPath       string `json:"lpath,omitempty"`
	PhysicalComponent string `json:"pcomp,omitempty"`
	Attempt           uint32 `json:"att,omitempty"`

	// File state records ("partial", "delivered", "retired")
	FileID        string          `json:"fid,omitempty"`
	Path          string          `json:"path,omitempty"`
	Revision      string          `json:"rev,omitempty"`
	Metadata      string          `json:"meta,omitempty"`
	ExactSize     uint64          `json:"size,omitempty"`
	PartName      string          `json:"part,omitempty"`
	Ranges        []content.Range `json:"ranges,omitempty"`
	CheckpointGen uint64          `json:"cgen,omitempty"`
	BlockedReason uint16          `json:"blk,omitempty"`
}

type taskMeta struct {
	Intent          transfer.ReceiveIntent
	SelectionDigest [transfer.SelectionSpecDigestBytes]byte
	Redownload      bool
	Lifecycle       string
	Attention       AttentionReason
	StateGen        uint64
}

type partialEntry struct {
	FileID        catalog.FileID
	Path          string
	Revision      content.FileRevision
	Metadata      content.SourceMetadata
	ExactSize     uint64
	PartName      string
	Ranges        []content.Range
	CheckpointGen uint64
	BlockedReason transfer.ItemBlockReason
}

type deliveredEntry struct {
	FileID    catalog.FileID
	Path      string
	Revision  content.FileRevision
	ExactSize uint64
}

type journalState struct {
	hasMeta   bool
	meta      taskMeta
	names     map[string]NameMapping
	partials  map[string]partialEntry
	delivered map[string]deliveredEntry
	lineCount int
	tornTail  bool
}

func newJournalState() journalState {
	return journalState{
		names:     make(map[string]NameMapping),
		partials:  make(map[string]partialEntry),
		delivered: make(map[string]deliveredEntry),
	}
}

func (s *journalState) apply(rec journalRecord) error {
	switch rec.Kind {
	case "meta":
		return s.applyMeta(rec)
	case "name":
		return s.applyName(rec)
	case "partial":
		return s.applyPartial(rec)
	case "delivered":
		return s.applyDelivered(rec)
	case "retired":
		delete(s.partials, rec.Path)
		return nil
	default:
		return fmt.Errorf("%w: unknown record kind %q", errJournalCorrupt, rec.Kind)
	}
}

func (s *journalState) applyMeta(rec journalRecord) error {
	if len(rec.Intent) > 0 {
		intent, err := transfer.DecodeReceiveIntent(rec.Intent)
		if err != nil {
			return fmt.Errorf("%w: decode intent: %w", errJournalCorrupt, err)
		}
		s.meta.Intent = intent
		s.meta.SelectionDigest = intent.SelectionSpec().Digest()
		s.meta.Redownload = rec.Redownload
		s.hasMeta = true
	}
	if rec.Lifecycle != "" {
		s.meta.Lifecycle = rec.Lifecycle
	}
	s.meta.Attention = AttentionReason(rec.Attention)
	if rec.StateGen > 0 {
		s.meta.StateGen = rec.StateGen
	}
	if !s.hasMeta || s.meta.StateGen == 0 {
		return fmt.Errorf("%w: missing meta header", errJournalCorrupt)
	}
	return nil
}

func (s *journalState) applyName(rec journalRecord) error {
	if rec.LogicalPath == "" || rec.PhysicalComponent == "" {
		return fmt.Errorf("%w: invalid name record", errJournalCorrupt)
	}
	s.names[rec.LogicalPath] = NameMapping{
		LogicalPath:       rec.LogicalPath,
		PhysicalComponent: rec.PhysicalComponent,
		Attempt:           rec.Attempt,
	}
	return nil
}

func (s *journalState) applyPartial(rec journalRecord) error {
	fid, err := decodeID16[catalog.FileID](rec.FileID)
	if err != nil {
		return err
	}
	rev, err := decodeID16[content.FileRevision](rec.Revision)
	if err != nil {
		return err
	}
	var meta content.SourceMetadata
	if rec.Metadata != "" {
		raw, err := hex.DecodeString(rec.Metadata)
		if err != nil {
			return fmt.Errorf("%w: metadata hex", errJournalCorrupt)
		}
		meta, err = content.SourceMetadataFromBytes(raw)
		if err != nil {
			return fmt.Errorf("%w: metadata bytes", errJournalCorrupt)
		}
	}
	if rec.Path == "" || rec.PartName == "" {
		return fmt.Errorf("%w: invalid partial record", errJournalCorrupt)
	}
	s.partials[rec.Path] = partialEntry{
		FileID:        fid,
		Path:          rec.Path,
		Revision:      rev,
		Metadata:      meta,
		ExactSize:     rec.ExactSize,
		PartName:      rec.PartName,
		Ranges:        slices.Clone(rec.Ranges),
		CheckpointGen: rec.CheckpointGen,
		BlockedReason: transfer.ItemBlockReason(rec.BlockedReason),
	}
	return nil
}

func (s *journalState) applyDelivered(rec journalRecord) error {
	fid, err := decodeID16[catalog.FileID](rec.FileID)
	if err != nil {
		return err
	}
	rev, err := decodeID16[content.FileRevision](rec.Revision)
	if err != nil {
		return err
	}
	if rec.Path == "" {
		return fmt.Errorf("%w: invalid delivered record", errJournalCorrupt)
	}
	delete(s.partials, rec.Path)
	s.delivered[rec.Path] = deliveredEntry{
		FileID:    fid,
		Path:      rec.Path,
		Revision:  rev,
		ExactSize: rec.ExactSize,
	}
	return nil
}

func decodeID16[T ~[catalog.IdentityBytes]byte](encoded string) (T, error) {
	var out T
	raw, err := hex.DecodeString(encoded)
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("%w: invalid 16-byte id", errJournalCorrupt)
	}
	copy(out[:], raw)
	return out, nil
}

func encodeJournalLine(rec journalRecord) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	sum := crc32.ChecksumIEEE(payload)
	line := make([]byte, 0, 10+len(payload))
	line = fmt.Appendf(line, "%08x ", sum)
	line = append(line, payload...)
	line = append(line, '\n')
	return line, nil
}

func parseJournalLine(line []byte) (journalRecord, error) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) < 10 || line[8] != ' ' {
		return journalRecord{}, errJournalCorrupt
	}
	want, err := strconv.ParseUint(string(line[:8]), 16, 32)
	if err != nil {
		return journalRecord{}, errJournalCorrupt
	}
	payload := line[9:]
	if crc32.ChecksumIEEE(payload) != uint32(want) {
		return journalRecord{}, errJournalCorrupt
	}
	var rec journalRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return journalRecord{}, fmt.Errorf("%w: %w", errJournalCorrupt, err)
	}
	return rec, nil
}

// readJournalState loads and validates state.log read-only. A torn trailing
// line without newline (from a mid-write crash) is tolerated; corruption in
// earlier lines or a missing meta header returns errJournalCorrupt.
func readJournalState(opDir *Dir) (journalState, int64, error) {
	state := newJournalState()
	f, err := openChildFile(opDir, journalFileName, os.O_RDONLY, 0)
	if err != nil {
		return state, 0, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 32*1024)
	var validBytes int64
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) > journalMaxLineBytes {
			return state, 0, errJournalCorrupt
		}
		if len(raw) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		hasNewline := len(raw) > 0 && raw[len(raw)-1] == '\n'
		trimmed := bytes.TrimSuffix(raw, []byte("\n"))
		if len(trimmed) == 0 && hasNewline {
			validBytes += int64(len(raw))
			continue
		}
		rec, parseErr := parseJournalLine(trimmed)
		if parseErr != nil {
			// Tolerate only an incomplete final record left by an abrupt crash.
			if errors.Is(readErr, io.EOF) && !hasNewline {
				state.tornTail = true
				break
			}
			// Also tolerate a corrupted final line only if at exact EOF with no following lines.
			if errors.Is(readErr, io.EOF) && state.hasMeta {
				state.tornTail = true
				break
			}
			return state, 0, parseErr
		}
		if err := state.apply(rec); err != nil {
			return state, 0, err
		}
		validBytes += int64(len(raw))
		state.lineCount++
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return state, 0, readErr
		}
	}
	if !state.hasMeta {
		return state, 0, errJournalCorrupt
	}
	return state, validBytes, nil
}

// taskJournal owns append-only persistence and compaction for one operation.
type taskJournal struct {
	mu             sync.Mutex
	opDir          *Dir
	file           *os.File
	state          journalState
	dirty          bool
	unflushedCount int
	lastSync       time.Time
	now            func() time.Time
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
	rec := metaToRecord(meta, true)
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
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return nil, err
	}
	j := &taskJournal{
		opDir:    opDir,
		file:     f,
		state:    state,
		lastSync: now(),
		now:      now,
	}
	if j.shouldCompactLocked() {
		if err := j.compactLocked(); err != nil {
			_ = j.file.Close()
			return nil, err
		}
	}
	return j, nil
}

func metaToRecord(meta taskMeta, includeIntent bool) journalRecord {
	rec := journalRecord{
		Kind:      "meta",
		Lifecycle: meta.Lifecycle,
		Attention: string(meta.Attention),
		StateGen:  meta.StateGen,
	}
	if includeIntent {
		rec.Intent = meta.Intent.CanonicalBytes()
		rec.SelectionDigest = hex.EncodeToString(meta.SelectionDigest[:])
		rec.Redownload = meta.Redownload
	}
	return rec
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

func (j *taskJournal) recordLifecycle(lifecycle string, attention AttentionReason) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	next := j.state.meta
	next.Lifecycle = lifecycle
	next.Attention = attention
	next.StateGen++
	return j.appendLocked(metaToRecord(next, false), true)
}

func (j *taskJournal) recordName(mapping NameMapping) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(journalRecord{
		Kind:              "name",
		LogicalPath:       mapping.LogicalPath,
		PhysicalComponent: mapping.PhysicalComponent,
		Attempt:           mapping.Attempt,
	}, false)
}

func (j *taskJournal) recordPartial(entry partialEntry, syncNow bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	var metaHex string
	if !entry.Metadata.IsZero() {
		metaHex = hex.EncodeToString(entry.Metadata.Bytes())
	}
	return j.appendLocked(journalRecord{
		Kind:          "partial",
		FileID:        hex.EncodeToString(entry.FileID.Bytes()),
		Path:          entry.Path,
		Revision:      hex.EncodeToString(entry.Revision.Bytes()),
		Metadata:      metaHex,
		ExactSize:     entry.ExactSize,
		PartName:      entry.PartName,
		Ranges:        entry.Ranges,
		CheckpointGen: entry.CheckpointGen,
		BlockedReason: uint16(entry.BlockedReason),
	}, syncNow)
}

func (j *taskJournal) recordDelivered(entry deliveredEntry, syncNow bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(journalRecord{
		Kind:      "delivered",
		FileID:    hex.EncodeToString(entry.FileID.Bytes()),
		Path:      entry.Path,
		Revision:  hex.EncodeToString(entry.Revision.Bytes()),
		ExactSize: entry.ExactSize,
	}, syncNow)
}

func (j *taskJournal) recordRetired(fileID catalog.FileID, path string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, exists := j.state.partials[path]; !exists {
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
	line, err := encodeJournalLine(rec)
	if err != nil {
		return err
	}
	if _, err := j.file.Write(line); err != nil {
		return err
	}
	if err := j.state.apply(rec); err != nil {
		return err
	}
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
		return j.compactLocked()
	}
	return nil
}

func (j *taskJournal) shouldCompactLocked() bool {
	live := 1 + len(j.state.names) + len(j.state.partials) + len(j.state.delivered)
	return j.state.lineCount >= journalCompactMinLines && j.state.lineCount > 2*live
}

func (j *taskJournal) compactLocked() error {
	const tmpName = "state.log.compact"
	_ = removeChild(j.opDir, tmpName, false)
	tmp, err := openChildFile(j.opDir, tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	writeRec := func(rec journalRecord) error {
		line, err := encodeJournalLine(rec)
		if err != nil {
			return err
		}
		buf.Write(line)
		return nil
	}
	if err := writeRec(metaToRecord(j.state.meta, true)); err != nil {
		_ = tmp.Close()
		_ = removeChild(j.opDir, tmpName, false)
		return err
	}
	lines := 1
	for _, m := range j.state.names {
		if err := writeRec(journalRecord{
			Kind:              "name",
			LogicalPath:       m.LogicalPath,
			PhysicalComponent: m.PhysicalComponent,
			Attempt:           m.Attempt,
		}); err != nil {
			_ = tmp.Close()
			_ = removeChild(j.opDir, tmpName, false)
			return err
		}
		lines++
	}
	for _, p := range j.state.partials {
		var metaHex string
		if !p.Metadata.IsZero() {
			metaHex = hex.EncodeToString(p.Metadata.Bytes())
		}
		if err := writeRec(journalRecord{
			Kind:          "partial",
			FileID:        hex.EncodeToString(p.FileID.Bytes()),
			Path:          p.Path,
			Revision:      hex.EncodeToString(p.Revision.Bytes()),
			Metadata:      metaHex,
			ExactSize:     p.ExactSize,
			PartName:      p.PartName,
			Ranges:        p.Ranges,
			CheckpointGen: p.CheckpointGen,
			BlockedReason: uint16(p.BlockedReason),
		}); err != nil {
			_ = tmp.Close()
			_ = removeChild(j.opDir, tmpName, false)
			return err
		}
		lines++
	}
	for _, d := range j.state.delivered {
		if err := writeRec(journalRecord{
			Kind:      "delivered",
			FileID:    hex.EncodeToString(d.FileID.Bytes()),
			Path:      d.Path,
			Revision:  hex.EncodeToString(d.Revision.Bytes()),
			ExactSize: d.ExactSize,
		}); err != nil {
			_ = tmp.Close()
			_ = removeChild(j.opDir, tmpName, false)
			return err
		}
		lines++
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
	if err := renameFileNoReplace(j.opDir, tmpName, j.opDir, journalFileName, true); err != nil {
		_ = removeChild(j.opDir, tmpName, false)
		return err
	}
	_ = j.opDir.Sync()
	reopened, err := openChildFile(j.opDir, journalFileName, os.O_RDWR, privateFileMode)
	if err != nil {
		return err
	}
	if _, err := reopened.Seek(0, io.SeekEnd); err != nil {
		_ = reopened.Close()
		return err
	}
	j.file = reopened
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

func removeOperationDirectory(root *Dir, opDirName string) error {
	ctrlPath := filepath.Join(root.path, ControlDirName)
	opPath := filepath.Join(ctrlPath, opDirName)
	if err := os.RemoveAll(opPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
