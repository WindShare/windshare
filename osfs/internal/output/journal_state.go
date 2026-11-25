package output

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"slices"
	"strconv"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

const (
	journalHeaderReadBufferBytes   = 4 * 1024
	journalStateReadBufferBytes    = 32 * 1024
	journalMaxLineBytes            = 4 << 20
	stageFingerprintBytes          = sha256.Size
	journalLifecycleActive         = "active"
	journalLifecycleStopped        = "share_stopped"
	journalLifecycleCleanupPending = "cleanup_pending"
	journalLifecycleAttn           = "attention"
)

var errJournalCorrupt = errors.New("osfs: task journal is corrupt")

type journalRecord struct {
	Kind string `json:"k"`

	// Meta record ("meta")
	Intent          json.RawMessage `json:"intent,omitempty"`
	SelectionDigest string          `json:"sel,omitempty"`
	Redownload      bool            `json:"redl,omitempty"`
	Lifecycle       string          `json:"life,omitempty"`
	Attention       string          `json:"attn,omitempty"`
	StateGen        uint64          `json:"sgen,omitempty"`

	// Name mapping record ("name")
	LogicalPath       string `json:"lpath,omitempty"`
	PhysicalComponent string `json:"pcomp,omitempty"`
	Attempt           uint32 `json:"att,omitempty"`
	Directory         bool   `json:"dir,omitempty"`

	// File state records ("partial", "publishing", "delivered", "retired")
	FileID        string          `json:"fid,omitempty"`
	Path          string          `json:"path,omitempty"`
	Revision      string          `json:"rev,omitempty"`
	Metadata      string          `json:"meta,omitempty"`
	ExactSize     uint64          `json:"size,omitempty"`
	PartName      string          `json:"part,omitempty"`
	Ranges        []content.Range `json:"ranges,omitempty"`
	CheckpointGen uint64          `json:"cgen,omitempty"`
	BlockedReason uint16          `json:"blk,omitempty"`
	Fingerprint   string          `json:"fp,omitempty"`
}

// taskHeader holds the immutable creation fields persisted on the first record
// of state.log (both at creation and after compaction). Mutable lifecycle
// fields (Lifecycle, Attention, StateGen) can be updated by later records and
// are excluded here so header-only readers cannot observe stale state.
type taskHeader struct {
	Intent          transfer.ReceiveIntent
	SelectionDigest [transfer.SelectionSpecDigestBytes]byte
	Redownload      bool
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
	Fingerprint   [stageFingerprintBytes]byte
}

// publishingEntry records a completed staged file just before its no-replace
// rename so restart recovery can reconcile a lost delivery receipt without
// re-downloading or overwriting a foreign destination file.
type publishingEntry struct {
	FileID        catalog.FileID
	Path          string
	Revision      content.FileRevision
	Metadata      content.SourceMetadata
	ExactSize     uint64
	PartName      string
	CheckpointGen uint64
	Fingerprint   [stageFingerprintBytes]byte
}

type deliveredEntry struct {
	FileID    catalog.FileID
	Path      string
	Revision  content.FileRevision
	ExactSize uint64
}

type journalState struct {
	hasMeta    bool
	meta       taskMeta
	names      map[string]NameMapping
	partials   map[string]partialEntry
	publishing map[string]publishingEntry
	delivered  map[string]deliveredEntry
	lineCount  int
	tornTail   bool
}

func newJournalState() journalState {
	return journalState{
		names:      make(map[string]NameMapping),
		partials:   make(map[string]partialEntry),
		publishing: make(map[string]publishingEntry),
		delivered:  make(map[string]deliveredEntry),
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
	case "publishing":
		return s.applyPublishing(rec)
	case "delivered":
		return s.applyDelivered(rec)
	case "retired":
		delete(s.partials, rec.Path)
		delete(s.publishing, rec.Path)
		return nil
	default:
		return fmt.Errorf("%w: unknown record kind %q", errJournalCorrupt, rec.Kind)
	}
}

func decodeTaskHeader(rec journalRecord) (taskHeader, error) {
	if rec.Kind != "meta" || len(rec.Intent) == 0 || rec.StateGen == 0 {
		return taskHeader{}, fmt.Errorf("%w: missing meta header", errJournalCorrupt)
	}
	var intent transfer.ReceiveIntent
	if err := json.Unmarshal(rec.Intent, &intent); err != nil {
		return taskHeader{}, fmt.Errorf("%w: decode intent: %w", errJournalCorrupt, err)
	}
	return taskHeader{
		Intent:          intent,
		SelectionDigest: intent.SelectionSpec().Digest(),
		Redownload:      rec.Redownload,
	}, nil
}

func (s *journalState) applyMeta(rec journalRecord) error {
	if len(rec.Intent) > 0 {
		header, err := decodeTaskHeader(rec)
		if err != nil {
			return err
		}
		s.meta.Intent = header.Intent
		s.meta.SelectionDigest = header.SelectionDigest
		s.meta.Redownload = header.Redownload
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
		Directory:         rec.Directory,
	}
	return nil
}

func (s *journalState) applyPartial(rec journalRecord) error {
	fid, rev, meta, err := decodeFileRecordIdentity(rec)
	if err != nil {
		return err
	}
	var fp [stageFingerprintBytes]byte
	if rec.Fingerprint != "" {
		fp, err = decodeFingerprint(rec.Fingerprint)
		if err != nil {
			return err
		}
	}
	if rec.Path == "" || rec.PartName == "" {
		return fmt.Errorf("%w: invalid partial record", errJournalCorrupt)
	}
	delete(s.publishing, rec.Path)
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
		Fingerprint:   fp,
	}
	return nil
}

func (s *journalState) applyPublishing(rec journalRecord) error {
	fid, rev, meta, err := decodeFileRecordIdentity(rec)
	if err != nil {
		return err
	}
	fp, err := decodeFingerprint(rec.Fingerprint)
	if err != nil {
		return err
	}
	if rec.Path == "" || rec.PartName == "" || rec.CheckpointGen == 0 {
		return fmt.Errorf("%w: invalid publishing record", errJournalCorrupt)
	}
	delete(s.partials, rec.Path)
	s.publishing[rec.Path] = publishingEntry{
		FileID:        fid,
		Path:          rec.Path,
		Revision:      rev,
		Metadata:      meta,
		ExactSize:     rec.ExactSize,
		PartName:      rec.PartName,
		CheckpointGen: rec.CheckpointGen,
		Fingerprint:   fp,
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
	delete(s.publishing, rec.Path)
	s.delivered[rec.Path] = deliveredEntry{
		FileID:    fid,
		Path:      rec.Path,
		Revision:  rev,
		ExactSize: rec.ExactSize,
	}
	return nil
}

func decodeFileRecordIdentity(rec journalRecord) (catalog.FileID, content.FileRevision, content.SourceMetadata, error) {
	fid, err := decodeID16[catalog.FileID](rec.FileID)
	if err != nil {
		return catalog.FileID{}, content.FileRevision{}, content.SourceMetadata{}, err
	}
	rev, err := decodeID16[content.FileRevision](rec.Revision)
	if err != nil {
		return catalog.FileID{}, content.FileRevision{}, content.SourceMetadata{}, err
	}
	var meta content.SourceMetadata
	if rec.Metadata != "" {
		raw, err := hex.DecodeString(rec.Metadata)
		if err != nil {
			return catalog.FileID{}, content.FileRevision{}, content.SourceMetadata{}, fmt.Errorf("%w: metadata hex", errJournalCorrupt)
		}
		meta, err = content.SourceMetadataFromBytes(raw)
		if err != nil {
			return catalog.FileID{}, content.FileRevision{}, content.SourceMetadata{}, fmt.Errorf("%w: metadata bytes", errJournalCorrupt)
		}
	}
	return fid, rev, meta, nil
}

func decodeFingerprint(encoded string) ([stageFingerprintBytes]byte, error) {
	var out [stageFingerprintBytes]byte
	raw, err := hex.DecodeString(encoded)
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("%w: invalid stage fingerprint", errJournalCorrupt)
	}
	copy(out[:], raw)
	if out == ([stageFingerprintBytes]byte{}) {
		return out, fmt.Errorf("%w: zero stage fingerprint", errJournalCorrupt)
	}
	return out, nil
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

func metaToRecord(meta taskMeta, includeIntent bool) (journalRecord, error) {
	rec := journalRecord{
		Kind:      "meta",
		Lifecycle: meta.Lifecycle,
		Attention: string(meta.Attention),
		StateGen:  meta.StateGen,
	}
	if includeIntent {
		encoded, err := json.Marshal(meta.Intent)
		if err != nil {
			return journalRecord{}, err
		}
		rec.Intent = encoded
		rec.SelectionDigest = hex.EncodeToString(meta.SelectionDigest[:])
		rec.Redownload = meta.Redownload
	}
	return rec, nil
}

func nameToRecord(m NameMapping) journalRecord {
	return journalRecord{
		Kind:              "name",
		LogicalPath:       m.LogicalPath,
		PhysicalComponent: m.PhysicalComponent,
		Attempt:           m.Attempt,
		Directory:         m.Directory,
	}
}

func partialToRecord(entry partialEntry) journalRecord {
	var metaHex string
	if !entry.Metadata.IsZero() {
		metaHex = hex.EncodeToString(entry.Metadata.Bytes())
	}
	var fpHex string
	if entry.Fingerprint != ([stageFingerprintBytes]byte{}) {
		fpHex = hex.EncodeToString(entry.Fingerprint[:])
	}
	return journalRecord{
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
		Fingerprint:   fpHex,
	}
}

func publishingToRecord(entry publishingEntry) journalRecord {
	var metaHex string
	if !entry.Metadata.IsZero() {
		metaHex = hex.EncodeToString(entry.Metadata.Bytes())
	}
	return journalRecord{
		Kind:          "publishing",
		FileID:        hex.EncodeToString(entry.FileID.Bytes()),
		Path:          entry.Path,
		Revision:      hex.EncodeToString(entry.Revision.Bytes()),
		Metadata:      metaHex,
		ExactSize:     entry.ExactSize,
		PartName:      entry.PartName,
		CheckpointGen: entry.CheckpointGen,
		Fingerprint:   hex.EncodeToString(entry.Fingerprint[:]),
	}
}

func deliveredToRecord(entry deliveredEntry) journalRecord {
	return journalRecord{
		Kind:      "delivered",
		FileID:    hex.EncodeToString(entry.FileID.Bytes()),
		Path:      entry.Path,
		Revision:  hex.EncodeToString(entry.Revision.Bytes()),
		ExactSize: entry.ExactSize,
	}
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

// readJournalHeader reads only the immutable first record of state.log without
// replaying file-level progress entries. Callers that need mutable lifecycle
// state, full integrity validation, or per-file checkpoints must use
// readJournalState.
func readJournalHeader(opDir *Dir) (taskHeader, error) {
	f, err := openJournalHeaderFile(opDir)
	if err != nil {
		return taskHeader{}, err
	}
	defer f.Close()

	// Bound total bytes read so a corrupt file without a newline cannot force
	// unbounded allocation during directory-wide scans.
	reader := bufio.NewReaderSize(io.LimitReader(f, journalMaxLineBytes+1), journalHeaderReadBufferBytes)
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) > journalMaxLineBytes {
			return taskHeader{}, errJournalCorrupt
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return taskHeader{}, readErr
		}
		if len(raw) == 0 && errors.Is(readErr, io.EOF) {
			return taskHeader{}, fmt.Errorf("%w: missing meta header", errJournalCorrupt)
		}
		if raw[len(raw)-1] != '\n' {
			return taskHeader{}, fmt.Errorf("%w: incomplete meta header", errJournalCorrupt)
		}
		trimmed := raw[:len(raw)-1]
		if len(trimmed) == 0 {
			continue
		}
		rec, parseErr := parseJournalLine(trimmed)
		if parseErr != nil {
			return taskHeader{}, parseErr
		}
		return decodeTaskHeader(rec)
	}
}

// readJournalState loads and validates state.log read-only. Every committed
// record is newline-terminated; any trailing bytes without a newline (from a
// mid-write crash, even if the JSON payload and CRC happen to be intact) are
// treated as an uncommitted torn tail to be truncated before appending.
// Corruption in earlier lines or a missing meta header returns errJournalCorrupt.
func readJournalState(opDir *Dir) (journalState, int64, error) {
	state := newJournalState()
	f, err := openChildFile(opDir, journalFileName, os.O_RDONLY, 0)
	if err != nil {
		return state, 0, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, journalStateReadBufferBytes)
	var validBytes int64
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) > journalMaxLineBytes {
			return state, 0, errJournalCorrupt
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return state, 0, readErr
		}
		if len(raw) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if raw[len(raw)-1] != '\n' {
			// Newline is the frame terminator: without it the trailing write
			// never completed, and accepting it would cause the next append to
			// concatenate onto this line and corrupt state.log.
			state.tornTail = true
			break
		}
		trimmed := raw[:len(raw)-1]
		if len(trimmed) == 0 {
			validBytes += int64(len(raw))
			continue
		}
		rec, parseErr := parseJournalLine(trimmed)
		if parseErr != nil {
			return state, 0, parseErr
		}
		if err := state.apply(rec); err != nil {
			return state, 0, err
		}
		validBytes += int64(len(raw))
		state.lineCount++
	}
	if !state.hasMeta {
		return state, 0, errJournalCorrupt
	}
	return state, validBytes, nil
}
