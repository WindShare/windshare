package transfer

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/windshare/windshare/core/catalog"
)

const (
	selectionPlanSortMemoryBytes = uint64(8) << 20
	maximumSelectionPlanBytes    = uint64(512) << 20
	selectionPlanFrameBytes      = 2 * 4
	selectionPlanDirectoryKind   = byte(1)
	selectionPlanFileKind        = byte(2)
	selectionPlanActiveOffset    = int64(5)
	selectionClaimBytes          = catalog.IdentityBytes
	maximumSelectionClaims       = catalog.DefaultShareCommittedEntries + 1
	maximumSelectionRecordBytes  = catalog.MaxPathBytes + 6 + 3*catalog.IdentityBytes + 8 + 14
)

var (
	ErrSelectionPlanBudget = errors.New("transfer selection plan storage budget exceeded")
	ErrSelectionPlanState  = errors.New("transfer selection plan storage state is invalid")
)

type selectionPlanRecord struct {
	kind      byte
	active    bool
	path      string
	directory plannedDirectory
	file      plannedFile
}

type selectionDirectoryReference struct {
	activeOffset int64
}

type selectionSpoolCheckpoint struct {
	rawOffset   int64
	claimOffset int64
	rawBytes    uint64
	claimCount  uint64
	directories uint64
	files       uint64
}

type selectionSpool struct {
	mu sync.Mutex

	root        string
	rawPath     string
	planPath    string
	claimPath   string
	raw         *os.File
	claims      *os.File
	rawBytes    uint64
	claimCount  uint64
	directories uint64
	files       uint64
	frozen      bool
	closed      bool
}

func newSelectionSpool(share catalog.ShareInstance) (*selectionSpool, error) {
	if share.IsZero() {
		return nil, ErrSelectionPlanState
	}
	root, err := os.MkdirTemp("", fmt.Sprintf("windshare-selection-%x-", share.Bytes()[:4]))
	if err != nil {
		return nil, fmt.Errorf("create selection plan spool: %w", err)
	}
	spool := &selectionSpool{
		root: root, rawPath: filepath.Join(root, "discovery.plan"),
		planPath: filepath.Join(root, "terminal.plan"), claimPath: filepath.Join(root, "identities.claims"),
	}
	defer func() {
		if err != nil {
			_ = spool.Close()
		}
	}()
	spool.raw, err = secureSelectionFile(spool.rawPath)
	if err != nil {
		return nil, err
	}
	spool.claims, err = secureSelectionFile(spool.claimPath)
	if err != nil {
		return nil, err
	}
	return spool, nil
}

func secureSelectionFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create selection plan file: %w", err)
	}
	return file, nil
}

func (spool *selectionSpool) claim(identity catalog.NodeID) error {
	spool.mu.Lock()
	defer spool.mu.Unlock()
	if spool.closed || spool.frozen || identity.IsZero() {
		return ErrSelectionPlanState
	}
	if spool.claimCount >= maximumSelectionClaims {
		return NewJobResourceBudgetError(ErrSelectionPlanBudget)
	}
	if _, err := spool.claims.Write(identity.Bytes()); err != nil {
		return fmt.Errorf("write selection identity claim: %w", err)
	}
	spool.claimCount++
	return nil
}

func (spool *selectionSpool) appendDirectory(
	directory plannedDirectory,
) (selectionDirectoryReference, error) {
	record := selectionPlanRecord{
		kind: selectionPlanDirectoryKind, path: directory.path, directory: directory,
	}
	spool.mu.Lock()
	defer spool.mu.Unlock()
	start, err := spool.appendRecordLocked(record)
	return selectionDirectoryReference{activeOffset: start + selectionPlanActiveOffset}, err
}

func (spool *selectionSpool) requireDirectory(reference selectionDirectoryReference) error {
	spool.mu.Lock()
	defer spool.mu.Unlock()
	if spool.closed || spool.frozen || reference.activeOffset < selectionPlanActiveOffset {
		return ErrSelectionPlanState
	}
	var active [1]byte
	if _, err := spool.raw.ReadAt(active[:], reference.activeOffset); err != nil {
		return fmt.Errorf("read selection directory marker: %w", err)
	}
	if active[0] == 1 {
		return nil
	}
	if active[0] != 0 {
		return ErrSelectionPlanState
	}
	active[0] = 1
	if _, err := spool.raw.WriteAt(active[:], reference.activeOffset); err != nil {
		return fmt.Errorf("mark required selection directory: %w", err)
	}
	spool.directories++
	return nil
}

func (spool *selectionSpool) appendFile(file plannedFile) error {
	spool.mu.Lock()
	defer spool.mu.Unlock()
	_, err := spool.appendRecordLocked(selectionPlanRecord{
		kind: selectionPlanFileKind, active: true, path: file.path, file: file,
	})
	if err == nil {
		spool.files++
	}
	return err
}

func (spool *selectionSpool) appendRecordLocked(record selectionPlanRecord) (int64, error) {
	if spool.closed || spool.frozen || !validSelectionPath(record.path) {
		return 0, ErrSelectionPlanState
	}
	payload, err := encodeSelectionPlanRecord(record)
	if err != nil {
		return 0, err
	}
	frameBytes := uint64(len(payload) + selectionPlanFrameBytes)
	if frameBytes > maximumSelectionPlanBytes-spool.rawBytes {
		return 0, NewJobResourceBudgetError(ErrSelectionPlanBudget)
	}
	start, err := spool.raw.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, fmt.Errorf("locate selection plan append: %w", err)
	}
	if err := writeSelectionPlanFrame(spool.raw, payload); err != nil {
		return 0, err
	}
	spool.rawBytes += frameBytes
	return start, nil
}

func (spool *selectionSpool) checkpoint() (selectionSpoolCheckpoint, error) {
	spool.mu.Lock()
	defer spool.mu.Unlock()
	if spool.closed || spool.frozen {
		return selectionSpoolCheckpoint{}, ErrSelectionPlanState
	}
	rawOffset, err := spool.raw.Seek(0, io.SeekCurrent)
	if err != nil {
		return selectionSpoolCheckpoint{}, fmt.Errorf("locate selection plan checkpoint: %w", err)
	}
	claimOffset, err := spool.claims.Seek(0, io.SeekCurrent)
	if err != nil {
		return selectionSpoolCheckpoint{}, fmt.Errorf("locate selection claim checkpoint: %w", err)
	}
	if rawOffset < 0 || uint64(rawOffset) != spool.rawBytes || claimOffset < 0 ||
		uint64(claimOffset) != spool.claimCount*selectionClaimBytes {
		return selectionSpoolCheckpoint{}, ErrSelectionPlanState
	}
	return selectionSpoolCheckpoint{
		rawOffset: rawOffset, claimOffset: claimOffset,
		rawBytes: spool.rawBytes, claimCount: spool.claimCount,
		directories: spool.directories, files: spool.files,
	}, nil
}

func (spool *selectionSpool) rollback(checkpoint selectionSpoolCheckpoint) error {
	spool.mu.Lock()
	defer spool.mu.Unlock()
	if spool.closed || spool.frozen || checkpoint.rawOffset < 0 || checkpoint.claimOffset < 0 ||
		checkpoint.rawBytes > spool.rawBytes || checkpoint.rawOffset != int64(checkpoint.rawBytes) ||
		checkpoint.claimCount > spool.claimCount ||
		checkpoint.claimOffset != int64(checkpoint.claimCount*selectionClaimBytes) ||
		checkpoint.directories > spool.directories || checkpoint.files > spool.files {
		return ErrSelectionPlanState
	}
	if err := spool.raw.Truncate(checkpoint.rawOffset); err != nil {
		return fmt.Errorf("truncate failed directory selection plan: %w", err)
	}
	if _, err := spool.raw.Seek(checkpoint.rawOffset, io.SeekStart); err != nil {
		return fmt.Errorf("restore selection plan append position: %w", err)
	}
	if err := spool.claims.Truncate(checkpoint.claimOffset); err != nil {
		return fmt.Errorf("truncate failed directory selection claims: %w", err)
	}
	if _, err := spool.claims.Seek(checkpoint.claimOffset, io.SeekStart); err != nil {
		return fmt.Errorf("restore selection claim append position: %w", err)
	}
	spool.rawBytes = checkpoint.rawBytes
	spool.claimCount = checkpoint.claimCount
	spool.directories = checkpoint.directories
	spool.files = checkpoint.files
	return nil
}

func (spool *selectionSpool) Freeze(ctx context.Context) error {
	spool.mu.Lock()
	if spool.closed || spool.frozen {
		spool.mu.Unlock()
		return ErrSelectionPlanState
	}
	spool.frozen = true
	spool.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := spool.raw.Sync(); err != nil {
		return fmt.Errorf("sync discovered selection plan: %w", err)
	}
	if err := spool.claims.Sync(); err != nil {
		return fmt.Errorf("sync selection identity claims: %w", err)
	}
	if err := spool.rejectDuplicateClaims(ctx); err != nil {
		return err
	}
	if err := spool.sortTerminalPlan(ctx); err != nil {
		return err
	}
	if err := errors.Join(spool.raw.Close(), spool.claims.Close()); err != nil {
		return fmt.Errorf("close selection discovery files: %w", err)
	}
	spool.raw = nil
	spool.claims = nil
	if err := errors.Join(os.Remove(spool.rawPath), os.Remove(spool.claimPath)); err != nil {
		return fmt.Errorf("remove selection discovery files: %w", err)
	}
	return nil
}

func (spool *selectionSpool) DirectoryCount() uint64 { return spool.directories }
func (spool *selectionSpool) FileCount() uint64      { return spool.files }

func (spool *selectionSpool) VisitRecords(visit func(selectionPlanRecord) error) error {
	return spool.visit(false, 0, visit)
}

func (spool *selectionSpool) VisitFiles(visit func(plannedFile) error) error {
	return spool.visit(false, selectionPlanFileKind, func(record selectionPlanRecord) error {
		return visit(record.file)
	})
}

func (spool *selectionSpool) VisitDirectoriesReverse(visit func(plannedDirectory) error) error {
	return spool.visit(true, selectionPlanDirectoryKind, func(record selectionPlanRecord) error {
		return visit(record.directory)
	})
}

func (spool *selectionSpool) visit(
	reverse bool,
	kind byte,
	visit func(selectionPlanRecord) error,
) error {
	spool.mu.Lock()
	if spool.closed || !spool.frozen {
		spool.mu.Unlock()
		return ErrSelectionPlanState
	}
	path := spool.planPath
	spool.mu.Unlock()
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open terminal selection plan: %w", err)
	}
	defer file.Close()
	if !reverse {
		reader := bufio.NewReader(file)
		for {
			record, readErr := readSelectionPlanFrame(reader)
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
			if kind != 0 && record.kind != kind {
				continue
			}
			if err := visit(record); err != nil {
				return err
			}
		}
	}
	position, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	for position > 0 {
		record, start, readErr := readSelectionPlanFrameAt(file, position)
		if readErr != nil {
			return readErr
		}
		position = start
		if kind != 0 && record.kind != kind {
			continue
		}
		if err := visit(record); err != nil {
			return err
		}
	}
	return nil
}

func (spool *selectionSpool) Close() error {
	spool.mu.Lock()
	if spool.closed {
		spool.mu.Unlock()
		return nil
	}
	spool.closed = true
	raw, claims, root := spool.raw, spool.claims, spool.root
	spool.raw, spool.claims = nil, nil
	spool.mu.Unlock()
	var err error
	if raw != nil {
		err = errors.Join(err, raw.Close())
	}
	if claims != nil {
		err = errors.Join(err, claims.Close())
	}
	if removeErr := os.RemoveAll(root); removeErr != nil {
		err = errors.Join(err, fmt.Errorf("remove selection plan spool: %w", removeErr))
	}
	return err
}

func encodeSelectionPlanRecord(record selectionPlanRecord) ([]byte, error) {
	if !validSelectionPath(record.path) || (record.kind != selectionPlanDirectoryKind && record.kind != selectionPlanFileKind) {
		return nil, ErrSelectionPlanState
	}
	path := []byte(record.path)
	payload := make([]byte, 0, 96+len(path))
	payload = append(payload, record.kind)
	active := byte(0)
	if record.active {
		active = 1
	}
	payload = append(payload, active)
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(path)))
	payload = append(payload, path...)
	switch record.kind {
	case selectionPlanDirectoryKind:
		if record.directory.directory.IsZero() || record.directory.generation.IsZero() {
			return nil, ErrSelectionPlanState
		}
		payload = append(payload, record.directory.directory.Bytes()...)
		payload = append(payload, record.directory.generation.Bytes()...)
		payload = appendSelectionModifiedTime(payload, record.directory.modified)
	case selectionPlanFileKind:
		if record.file.file.IsZero() || record.file.parentDirectory.IsZero() || record.file.parentGeneration.IsZero() ||
			record.file.expectedSize > catalog.MaxFileSize {
			return nil, ErrSelectionPlanState
		}
		payload = append(payload, record.file.file.Bytes()...)
		payload = append(payload, record.file.parentDirectory.Bytes()...)
		payload = append(payload, record.file.parentGeneration.Bytes()...)
		payload = binary.BigEndian.AppendUint64(payload, record.file.expectedSize)
		payload = appendSelectionModifiedTime(payload, record.file.modified)
	}
	return payload, nil
}

func appendSelectionModifiedTime(destination []byte, modified catalog.ModifiedTime) []byte {
	present := byte(0)
	if modified.Present() {
		present = 1
	}
	destination = append(destination, present)
	destination = binary.BigEndian.AppendUint64(destination, uint64(modified.Seconds()))
	destination = binary.BigEndian.AppendUint32(destination, modified.Nanoseconds())
	return append(destination, byte(modified.Precision()))
}

func decodeSelectionPlanRecord(payload []byte) (selectionPlanRecord, error) {
	if len(payload) < 6 {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	record := selectionPlanRecord{kind: payload[0], active: payload[1] == 1}
	if payload[1] > 1 {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	pathBytes := int(binary.BigEndian.Uint32(payload[2:6]))
	if pathBytes <= 0 || pathBytes > len(payload)-6 {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	record.path = string(payload[6 : 6+pathBytes])
	if !validSelectionPath(record.path) {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	remaining := payload[6+pathBytes:]
	switch record.kind {
	case selectionPlanDirectoryKind:
		if len(remaining) != 2*catalog.IdentityBytes+14 {
			return selectionPlanRecord{}, ErrSelectionPlanState
		}
		directory, err := catalog.DirectoryIDFromBytes(remaining[:catalog.IdentityBytes])
		if err != nil {
			return selectionPlanRecord{}, err
		}
		generation, err := catalog.DirectoryGenerationFromBytes(remaining[catalog.IdentityBytes : 2*catalog.IdentityBytes])
		if err != nil {
			return selectionPlanRecord{}, err
		}
		if directory.IsZero() || generation.IsZero() {
			return selectionPlanRecord{}, ErrSelectionPlanState
		}
		modified, err := decodeSelectionModifiedTime(remaining[2*catalog.IdentityBytes:])
		if err != nil {
			return selectionPlanRecord{}, err
		}
		record.directory = plannedDirectory{
			directory: directory, generation: generation, path: record.path, modified: modified,
		}
	case selectionPlanFileKind:
		if !record.active || len(remaining) != 3*catalog.IdentityBytes+8+14 {
			return selectionPlanRecord{}, ErrSelectionPlanState
		}
		file, err := catalog.FileIDFromBytes(remaining[:catalog.IdentityBytes])
		if err != nil {
			return selectionPlanRecord{}, err
		}
		parent, err := catalog.DirectoryIDFromBytes(remaining[catalog.IdentityBytes : 2*catalog.IdentityBytes])
		if err != nil {
			return selectionPlanRecord{}, err
		}
		generation, err := catalog.DirectoryGenerationFromBytes(remaining[2*catalog.IdentityBytes : 3*catalog.IdentityBytes])
		if err != nil {
			return selectionPlanRecord{}, err
		}
		expected := binary.BigEndian.Uint64(remaining[3*catalog.IdentityBytes : 3*catalog.IdentityBytes+8])
		if file.IsZero() || parent.IsZero() || generation.IsZero() || expected > catalog.MaxFileSize {
			return selectionPlanRecord{}, ErrSelectionPlanState
		}
		modified, err := decodeSelectionModifiedTime(remaining[3*catalog.IdentityBytes+8:])
		if err != nil {
			return selectionPlanRecord{}, err
		}
		record.file = plannedFile{
			file: file, path: record.path, expectedSize: expected, modified: modified,
			parentDirectory: parent, parentGeneration: generation,
		}
	default:
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	return record, nil
}

func decodeSelectionModifiedTime(encoded []byte) (catalog.ModifiedTime, error) {
	if len(encoded) != 14 || encoded[0] > 1 {
		return catalog.ModifiedTime{}, ErrSelectionPlanState
	}
	if encoded[0] == 0 {
		if !bytes.Equal(encoded[1:], make([]byte, 13)) {
			return catalog.ModifiedTime{}, ErrSelectionPlanState
		}
		return catalog.ModifiedTime{}, nil
	}
	return catalog.NewModifiedTime(
		int64(binary.BigEndian.Uint64(encoded[1:9])),
		binary.BigEndian.Uint32(encoded[9:13]),
		catalog.TimePrecision(encoded[13]),
	)
}

func writeSelectionPlanFrame(writer io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > maximumSelectionRecordBytes {
		return ErrSelectionPlanState
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	if _, err := writer.Write(length[:]); err != nil {
		return fmt.Errorf("write selection plan frame length: %w", err)
	}
	if _, err := writer.Write(payload); err != nil {
		return fmt.Errorf("write selection plan frame: %w", err)
	}
	if _, err := writer.Write(length[:]); err != nil {
		return fmt.Errorf("write selection plan frame suffix: %w", err)
	}
	return nil
}

func readSelectionPlanFrame(reader io.Reader) (selectionPlanRecord, error) {
	var length [4]byte
	if _, err := io.ReadFull(reader, length[:]); err != nil {
		return selectionPlanRecord{}, err
	}
	size := binary.BigEndian.Uint32(length[:])
	if size == 0 || size > maximumSelectionRecordBytes {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return selectionPlanRecord{}, fmt.Errorf("read selection plan frame: %w", err)
	}
	var suffix [4]byte
	if _, err := io.ReadFull(reader, suffix[:]); err != nil || suffix != length {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	return decodeSelectionPlanRecord(payload)
}

func readSelectionPlanFrameAt(file *os.File, end int64) (selectionPlanRecord, int64, error) {
	if end < selectionPlanFrameBytes {
		return selectionPlanRecord{}, 0, ErrSelectionPlanState
	}
	var suffix [4]byte
	if _, err := file.ReadAt(suffix[:], end-4); err != nil {
		return selectionPlanRecord{}, 0, err
	}
	size := int64(binary.BigEndian.Uint32(suffix[:]))
	start := end - size - selectionPlanFrameBytes
	if size <= 0 || size > maximumSelectionRecordBytes || start < 0 {
		return selectionPlanRecord{}, 0, ErrSelectionPlanState
	}
	payload := make([]byte, size)
	var prefix [4]byte
	if _, err := file.ReadAt(prefix[:], start); err != nil || prefix != suffix {
		return selectionPlanRecord{}, 0, ErrSelectionPlanState
	}
	if _, err := file.ReadAt(payload, start+4); err != nil {
		return selectionPlanRecord{}, 0, err
	}
	record, err := decodeSelectionPlanRecord(payload)
	return record, start, err
}

type selectionPlanRun struct {
	file   *os.File
	reader *bufio.Reader
	record selectionPlanRecord
	index  int
}

type selectionPlanRunHeap []*selectionPlanRun

func (runs selectionPlanRunHeap) Len() int { return len(runs) }
func (runs selectionPlanRunHeap) Less(left, right int) bool {
	if runs[left].record.path != runs[right].record.path {
		return runs[left].record.path < runs[right].record.path
	}
	return runs[left].record.kind < runs[right].record.kind
}
func (runs selectionPlanRunHeap) Swap(left, right int) {
	runs[left], runs[right] = runs[right], runs[left]
}
func (runs *selectionPlanRunHeap) Push(value any) { *runs = append(*runs, value.(*selectionPlanRun)) }
func (runs *selectionPlanRunHeap) Pop() any {
	old := *runs
	last := old[len(old)-1]
	*runs = old[:len(old)-1]
	return last
}

func (spool *selectionSpool) sortTerminalPlan(ctx context.Context) error {
	if _, err := spool.raw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReader(spool.raw)
	var runPaths []string
	defer func() {
		for _, path := range runPaths {
			_ = os.Remove(path)
		}
	}()
	for {
		var records []selectionPlanRecord
		var used uint64
		for used < selectionPlanSortMemoryBytes {
			if err := ctx.Err(); err != nil {
				return err
			}
			record, err := readSelectionPlanFrame(reader)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if record.kind == selectionPlanDirectoryKind && !record.active {
				continue
			}
			record.active = true
			recordBytes, err := encodeSelectionPlanRecord(record)
			if err != nil {
				return err
			}
			records = append(records, record)
			used += uint64(len(recordBytes))
		}
		if len(records) == 0 {
			if used == 0 {
				// EOF or a tail containing only inactive directories.
				if _, err := reader.Peek(1); errors.Is(err, io.EOF) {
					break
				}
				continue
			}
		}
		sort.Slice(records, func(left, right int) bool {
			if records[left].path != records[right].path {
				return records[left].path < records[right].path
			}
			return records[left].kind < records[right].kind
		})
		run, err := os.CreateTemp(spool.root, "plan-run-")
		if err != nil {
			return err
		}
		path := run.Name()
		runPaths = append(runPaths, path)
		if err := run.Chmod(0o600); err != nil {
			_ = run.Close()
			return err
		}
		for _, record := range records {
			payload, err := encodeSelectionPlanRecord(record)
			if err != nil {
				_ = run.Close()
				return err
			}
			if err := writeSelectionPlanFrame(run, payload); err != nil {
				_ = run.Close()
				return err
			}
		}
		if err := run.Close(); err != nil {
			return err
		}
		if _, err := reader.Peek(1); errors.Is(err, io.EOF) {
			break
		}
	}
	terminal, err := secureSelectionFile(spool.planPath)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if terminal != nil {
			_ = terminal.Close()
		}
		if !committed {
			_ = os.Remove(spool.planPath)
		}
	}()
	runs := make(selectionPlanRunHeap, 0, len(runPaths))
	openedRuns := make([]*os.File, 0, len(runPaths))
	defer func() {
		for _, file := range openedRuns {
			_ = file.Close()
		}
	}()
	for index, path := range runPaths {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		openedRuns = append(openedRuns, file)
		run := &selectionPlanRun{file: file, reader: bufio.NewReader(file), index: index}
		run.record, err = readSelectionPlanFrame(run.reader)
		if errors.Is(err, io.EOF) {
			_ = file.Close()
			continue
		}
		if err != nil {
			_ = file.Close()
			return err
		}
		runs = append(runs, run)
	}
	heap.Init(&runs)
	previousPath := ""
	for runs.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		run := heap.Pop(&runs).(*selectionPlanRun)
		if run.record.path == previousPath {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		previousPath = run.record.path
		payload, err := encodeSelectionPlanRecord(run.record)
		if err != nil {
			return err
		}
		if err := writeSelectionPlanFrame(terminal, payload); err != nil {
			return err
		}
		run.record, err = readSelectionPlanFrame(run.reader)
		if errors.Is(err, io.EOF) {
			if closeErr := run.file.Close(); closeErr != nil {
				return closeErr
			}
			continue
		}
		if err != nil {
			return err
		}
		heap.Push(&runs, run)
	}
	if err := terminal.Sync(); err != nil {
		return err
	}
	if err := terminal.Close(); err != nil {
		return err
	}
	terminal = nil
	committed = true
	return nil
}

func (spool *selectionSpool) rejectDuplicateClaims(ctx context.Context) error {
	if _, err := spool.claims.Seek(0, io.SeekStart); err != nil {
		return err
	}
	claimsPerRun := int(selectionPlanSortMemoryBytes / selectionClaimBytes)
	var runPaths []string
	defer func() {
		for _, path := range runPaths {
			_ = os.Remove(path)
		}
	}()
	for {
		claims := make([]catalog.NodeID, 0, claimsPerRun)
		for len(claims) < claimsPerRun {
			var raw [selectionClaimBytes]byte
			_, err := io.ReadFull(spool.claims, raw[:])
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("read selection identity claim: %w", err)
			}
			claim, err := catalog.NodeIDFromBytes(raw[:])
			if err != nil {
				return err
			}
			claims = append(claims, claim)
		}
		if len(claims) == 0 {
			break
		}
		sort.Slice(claims, func(left, right int) bool {
			return bytes.Compare(claims[left][:], claims[right][:]) < 0
		})
		for index := 1; index < len(claims); index++ {
			if claims[index] == claims[index-1] {
				return NewSessionFailure(ErrCatalogIdentity)
			}
		}
		run, err := os.CreateTemp(spool.root, "claim-run-")
		if err != nil {
			return err
		}
		path := run.Name()
		runPaths = append(runPaths, path)
		if err := run.Chmod(0o600); err != nil {
			_ = run.Close()
			return err
		}
		for _, claim := range claims {
			if _, err := run.Write(claim[:]); err != nil {
				_ = run.Close()
				return err
			}
		}
		if err := run.Close(); err != nil {
			return err
		}
	}
	type claimRun struct {
		file  *os.File
		value catalog.NodeID
	}
	runs := make([]*claimRun, 0, len(runPaths))
	openedRuns := make([]*os.File, 0, len(runPaths))
	defer func() {
		for _, file := range openedRuns {
			_ = file.Close()
		}
	}()
	for _, path := range runPaths {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		openedRuns = append(openedRuns, file)
		run := &claimRun{file: file}
		if _, err := io.ReadFull(file, run.value[:]); err != nil {
			_ = file.Close()
			return err
		}
		runs = append(runs, run)
	}
	var previous catalog.NodeID
	havePrevious := false
	for len(runs) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		minimum := 0
		for index := 1; index < len(runs); index++ {
			if bytes.Compare(runs[index].value[:], runs[minimum].value[:]) < 0 {
				minimum = index
			}
		}
		current := runs[minimum].value
		if havePrevious && current == previous {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		previous, havePrevious = current, true
		_, err := io.ReadFull(runs[minimum].file, runs[minimum].value[:])
		if errors.Is(err, io.EOF) {
			if closeErr := runs[minimum].file.Close(); closeErr != nil {
				return closeErr
			}
			runs = append(runs[:minimum], runs[minimum+1:]...)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
