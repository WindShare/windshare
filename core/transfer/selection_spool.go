package transfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/catalog"
)

const (
	selectionPlanSortMemoryBytes       = uint64(8) << 20
	maximumSelectionPlanBytes          = uint64(512) << 20
	selectionPlanKindOffset            = 0
	selectionPlanActiveFieldOffset     = 1
	selectionPlanPathLengthOffset      = 2
	selectionPlanLengthBytes           = 4
	selectionPlanFrameBytes            = 2 * selectionPlanLengthBytes
	selectionPlanRecordHeaderBytes     = selectionPlanPathLengthOffset + selectionPlanLengthBytes
	selectionModifiedSecondsBytes      = 8
	selectionModifiedNanosecondsBytes  = 4
	selectionModifiedSecondsOffset     = 1
	selectionModifiedNanosecondsOffset = selectionModifiedSecondsOffset + selectionModifiedSecondsBytes
	selectionModifiedPrecisionOffset   = selectionModifiedNanosecondsOffset + selectionModifiedNanosecondsBytes
	selectionModifiedTimeBytes         = selectionModifiedPrecisionOffset + 1
	selectionExpectedSizeBytes         = 8
	selectionPlanDirectoryKind         = byte(1)
	selectionPlanFileKind              = byte(2)
	selectionPlanActiveOffset          = int64(selectionPlanLengthBytes + selectionPlanActiveFieldOffset)
	selectionClaimBytes                = catalog.IdentityBytes
	maximumSelectionClaims             = catalog.DefaultShareCommittedEntries + 1
	maximumSelectionRecordBytes        = catalog.MaxPathBytes + selectionPlanRecordHeaderBytes +
		3*catalog.IdentityBytes + selectionExpectedSizeBytes + selectionModifiedTimeBytes
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

func (spool *selectionSpool) sortTerminalPlan(ctx context.Context) error {
	sorter := selectionPlanExternalSorter{spool: spool, ctx: ctx}
	return sorter.sort()
}

func (spool *selectionSpool) rejectDuplicateClaims(ctx context.Context) error {
	sorter := selectionClaimExternalSorter{spool: spool}
	return sorter.rejectDuplicates(ctx)
}
