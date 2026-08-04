package transfer

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/windshare/windshare/core/catalog"
)

type selectionPlanRun struct {
	file   *os.File
	reader *bufio.Reader
	record selectionPlanRecord
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

type selectionPlanExternalSorter struct {
	spool    *selectionSpool
	ctx      context.Context
	runPaths []string
}

func (sorter *selectionPlanExternalSorter) sort() error {
	if _, err := sorter.spool.raw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	defer func() { removeSelectionSortRuns(sorter.runPaths) }()
	if err := sorter.buildRuns(bufio.NewReader(sorter.spool.raw)); err != nil {
		return err
	}
	return sorter.writeTerminalPlan()
}

func (sorter *selectionPlanExternalSorter) buildRuns(reader *bufio.Reader) error {
	for {
		records, exhausted, err := readSelectionPlanRunChunk(sorter.ctx, reader)
		if err != nil {
			return err
		}
		if len(records) != 0 {
			path, err := createSelectionPlanRun(sorter.spool.root, records)
			if err != nil {
				return err
			}
			sorter.runPaths = append(sorter.runPaths, path)
		}
		if exhausted {
			return nil
		}
	}
}

func readSelectionPlanRunChunk(
	ctx context.Context,
	reader *bufio.Reader,
) ([]selectionPlanRecord, bool, error) {
	var records []selectionPlanRecord
	var used uint64
	for used < selectionPlanSortMemoryBytes {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		record, err := readSelectionPlanFrame(reader)
		if errors.Is(err, io.EOF) {
			return records, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		if record.kind == selectionPlanDirectoryKind && !record.active {
			continue
		}
		record.active = true
		payload, err := encodeSelectionPlanRecord(record)
		if err != nil {
			return nil, false, err
		}
		records = append(records, record)
		used += uint64(len(payload))
	}
	return records, false, nil
}

func createSelectionPlanRun(root string, records []selectionPlanRecord) (string, error) {
	sort.Slice(records, func(left, right int) bool {
		if records[left].path != records[right].path {
			return records[left].path < records[right].path
		}
		return records[left].kind < records[right].kind
	})
	run, err := os.CreateTemp(root, "plan-run-")
	if err != nil {
		return "", err
	}
	path := run.Name()
	committed := false
	defer func() {
		if !committed {
			_ = run.Close()
			_ = os.Remove(path)
		}
	}()
	if err := run.Chmod(0o600); err != nil {
		return "", err
	}
	for _, record := range records {
		if err := writeSelectionPlanRecord(run, record); err != nil {
			return "", err
		}
	}
	if err := run.Close(); err != nil {
		return "", err
	}
	committed = true
	return path, nil
}

func writeSelectionPlanRecord(writer io.Writer, record selectionPlanRecord) error {
	payload, err := encodeSelectionPlanRecord(record)
	if err != nil {
		return err
	}
	return writeSelectionPlanFrame(writer, payload)
}

func (sorter *selectionPlanExternalSorter) writeTerminalPlan() error {
	terminal, err := secureSelectionFile(sorter.spool.planPath)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if terminal != nil {
			_ = terminal.Close()
		}
		if !committed {
			_ = os.Remove(sorter.spool.planPath)
		}
	}()
	if err := sorter.mergeRuns(terminal); err != nil {
		return err
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

func (sorter *selectionPlanExternalSorter) mergeRuns(terminal io.Writer) error {
	runs, err := openSelectionPlanRuns(sorter.runPaths)
	if err != nil {
		return err
	}
	defer runs.close()
	previousPath := ""
	for runs.queue.Len() > 0 {
		if err := sorter.ctx.Err(); err != nil {
			return err
		}
		run := heap.Pop(&runs.queue).(*selectionPlanRun)
		if run.record.path == previousPath {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		previousPath = run.record.path
		if err := writeSelectionPlanRecord(terminal, run.record); err != nil {
			return err
		}
		exhausted, err := advanceSelectionPlanRun(run)
		if err != nil {
			return err
		}
		if !exhausted {
			heap.Push(&runs.queue, run)
		}
	}
	return nil
}

type selectionPlanRunSet struct {
	queue selectionPlanRunHeap
	files []*os.File
}

func openSelectionPlanRuns(paths []string) (*selectionPlanRunSet, error) {
	runs := &selectionPlanRunSet{
		queue: make(selectionPlanRunHeap, 0, len(paths)),
		files: make([]*os.File, 0, len(paths)),
	}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			runs.close()
			return nil, err
		}
		runs.files = append(runs.files, file)
		run := &selectionPlanRun{file: file, reader: bufio.NewReader(file)}
		run.record, err = readSelectionPlanFrame(run.reader)
		if errors.Is(err, io.EOF) {
			_ = file.Close()
			continue
		}
		if err != nil {
			_ = file.Close()
			runs.close()
			return nil, err
		}
		runs.queue = append(runs.queue, run)
	}
	heap.Init(&runs.queue)
	return runs, nil
}

func (runs *selectionPlanRunSet) close() {
	for _, file := range runs.files {
		_ = file.Close()
	}
}

func advanceSelectionPlanRun(run *selectionPlanRun) (bool, error) {
	record, err := readSelectionPlanFrame(run.reader)
	if errors.Is(err, io.EOF) {
		return true, run.file.Close()
	}
	if err != nil {
		return false, err
	}
	run.record = record
	return false, nil
}

type selectionClaimRun struct {
	file  *os.File
	value catalog.NodeID
}

type selectionClaimExternalSorter struct {
	spool    *selectionSpool
	runPaths []string
}

func (sorter *selectionClaimExternalSorter) rejectDuplicates(ctx context.Context) error {
	if _, err := sorter.spool.claims.Seek(0, io.SeekStart); err != nil {
		return err
	}
	defer func() { removeSelectionSortRuns(sorter.runPaths) }()
	if err := sorter.buildRuns(); err != nil {
		return err
	}
	runs, err := openSelectionClaimRuns(sorter.runPaths)
	if err != nil {
		return err
	}
	defer runs.close()
	return runs.rejectDuplicates(ctx)
}

func (sorter *selectionClaimExternalSorter) buildRuns() error {
	claimsPerRun := int(selectionPlanSortMemoryBytes / selectionClaimBytes)
	for {
		claims, exhausted, err := readSelectionClaimRunChunk(sorter.spool.claims, claimsPerRun)
		if err != nil {
			return err
		}
		if len(claims) == 0 {
			return nil
		}
		sort.Slice(claims, func(left, right int) bool {
			return bytes.Compare(claims[left][:], claims[right][:]) < 0
		})
		if selectionClaimsContainDuplicate(claims) {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		path, err := createSelectionClaimRun(sorter.spool.root, claims)
		if err != nil {
			return err
		}
		sorter.runPaths = append(sorter.runPaths, path)
		if exhausted {
			return nil
		}
	}
}

func readSelectionClaimRunChunk(
	reader io.Reader,
	limit int,
) ([]catalog.NodeID, bool, error) {
	claims := make([]catalog.NodeID, 0, limit)
	for len(claims) < limit {
		var raw [selectionClaimBytes]byte
		_, err := io.ReadFull(reader, raw[:])
		if errors.Is(err, io.EOF) {
			return claims, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("read selection identity claim: %w", err)
		}
		claim, err := catalog.NodeIDFromBytes(raw[:])
		if err != nil {
			return nil, false, err
		}
		claims = append(claims, claim)
	}
	return claims, false, nil
}

func selectionClaimsContainDuplicate(claims []catalog.NodeID) bool {
	for index := 1; index < len(claims); index++ {
		if claims[index] == claims[index-1] {
			return true
		}
	}
	return false
}

func createSelectionClaimRun(root string, claims []catalog.NodeID) (string, error) {
	run, err := os.CreateTemp(root, "claim-run-")
	if err != nil {
		return "", err
	}
	path := run.Name()
	committed := false
	defer func() {
		if !committed {
			_ = run.Close()
			_ = os.Remove(path)
		}
	}()
	if err := run.Chmod(0o600); err != nil {
		return "", err
	}
	for _, claim := range claims {
		if _, err := run.Write(claim[:]); err != nil {
			return "", err
		}
	}
	if err := run.Close(); err != nil {
		return "", err
	}
	committed = true
	return path, nil
}

type selectionClaimRunSet struct {
	runs  []*selectionClaimRun
	files []*os.File
}

func openSelectionClaimRuns(paths []string) (*selectionClaimRunSet, error) {
	runs := &selectionClaimRunSet{
		runs:  make([]*selectionClaimRun, 0, len(paths)),
		files: make([]*os.File, 0, len(paths)),
	}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			runs.close()
			return nil, err
		}
		runs.files = append(runs.files, file)
		run := &selectionClaimRun{file: file}
		if _, err := io.ReadFull(file, run.value[:]); err != nil {
			_ = file.Close()
			runs.close()
			return nil, err
		}
		runs.runs = append(runs.runs, run)
	}
	return runs, nil
}

func (runs *selectionClaimRunSet) close() {
	for _, file := range runs.files {
		_ = file.Close()
	}
}

func (runs *selectionClaimRunSet) rejectDuplicates(ctx context.Context) error {
	var previous catalog.NodeID
	havePrevious := false
	for len(runs.runs) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		minimum := runs.minimumIndex()
		current := runs.runs[minimum].value
		if havePrevious && current == previous {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		previous, havePrevious = current, true
		exhausted, err := advanceSelectionClaimRun(runs.runs[minimum])
		if err != nil {
			return err
		}
		if exhausted {
			runs.runs = append(runs.runs[:minimum], runs.runs[minimum+1:]...)
		}
	}
	return nil
}

func (runs *selectionClaimRunSet) minimumIndex() int {
	minimum := 0
	for index := 1; index < len(runs.runs); index++ {
		if bytes.Compare(runs.runs[index].value[:], runs.runs[minimum].value[:]) < 0 {
			minimum = index
		}
	}
	return minimum
}

func advanceSelectionClaimRun(run *selectionClaimRun) (bool, error) {
	_, err := io.ReadFull(run.file, run.value[:])
	if errors.Is(err, io.EOF) {
		return true, run.file.Close()
	}
	return false, err
}

func removeSelectionSortRuns(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}
