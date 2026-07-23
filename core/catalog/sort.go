package catalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	DefaultSortRunMemoryBytes = uint64(1) << 20
	sortRecordMemoryOverhead  = uint64(64)
	sortRecordHeaderBytes     = uint64(8)
)

type ResourceMeter interface {
	Consume(ResourceUsage) error
	Release(ResourceUsage) error
}

type ScannedChild struct {
	DirectoryID      DirectoryID
	FileID           FileID
	Name             string
	Locator          Locator
	SourceIdentity   SourceIdentity
	VersionCandidate VersionCandidate
	ExpectedSize     uint64
	ModifiedTime     ModifiedTime
}

func (child ScannedChild) nodeRecord(parent DirectoryID) (NodeRecord, error) {
	switch {
	case !child.DirectoryID.IsZero() && child.FileID.IsZero():
		if !child.VersionCandidate.IsZero() || child.ExpectedSize != 0 {
			return NodeRecord{}, errors.New("catalog scanned directory carries file metadata")
		}
		return NewDirectoryNodeRecord(
			child.DirectoryID, parent, child.Name, child.Locator, child.SourceIdentity, child.ModifiedTime,
		)
	case child.DirectoryID.IsZero() && !child.FileID.IsZero():
		return NewFileNodeRecord(
			child.FileID, parent, child.Name, child.Locator, child.SourceIdentity,
			child.VersionCandidate, child.ExpectedSize, child.ModifiedTime,
		)
	default:
		return NodeRecord{}, errors.New("catalog scanned child must have exactly one kind-safe identity")
	}
}

type sortRecord struct {
	key     []byte
	payload []byte
}

func (r sortRecord) memoryBytes() uint64 {
	return sortRecordMemoryOverhead + uint64(len(r.key)) + uint64(len(r.payload))
}

type externalSorter struct {
	workspace   SpillWorkspace
	meter       ResourceMeter
	hierarchy   BudgetHierarchy
	runBytes    uint64
	chunk       []sortRecord
	chunkBytes  uint64
	chunkBudget *BudgetReservation
	runs        []SpillObject
	count       uint64
}

func newExternalSorter(workspace SpillWorkspace, meter ResourceMeter, hierarchy BudgetHierarchy, runBytes uint64) (*externalSorter, error) {
	if workspace == nil || meter == nil {
		return nil, errors.New("catalog external sorter requires spill storage and resource meter")
	}
	if runBytes == 0 {
		runBytes = DefaultSortRunMemoryBytes
	}
	return &externalSorter{workspace: workspace, meter: meter, hierarchy: hierarchy, runBytes: runBytes}, nil
}

func (s *externalSorter) add(ctx context.Context, record sortRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	memory := record.memoryBytes()
	if s.chunkBytes != 0 && s.chunkBytes+memory > s.runBytes {
		if err := s.flush(ctx, true); err != nil {
			return err
		}
	}
	if s.chunkBudget == nil {
		reservation, err := ReserveHierarchy(s.hierarchy, ResourceUsage{})
		if err != nil {
			return err
		}
		s.chunkBudget = reservation
	}
	if err := s.chunkBudget.Grow(ResourceUsage{MemoryBytes: memory}); err != nil {
		return err
	}
	s.chunk = append(s.chunk, sortRecord{
		key: append([]byte(nil), record.key...), payload: append([]byte(nil), record.payload...),
	})
	s.chunkBytes += memory
	s.count++
	return nil
}

func (s *externalSorter) flush(ctx context.Context, rejectDuplicates bool) error {
	if len(s.chunk) == 0 {
		return nil
	}
	defer func() {
		s.chunkBudget.Release()
		s.chunkBudget = nil
		s.chunk = nil
		s.chunkBytes = 0
	}()
	sort.Slice(s.chunk, func(left, right int) bool {
		return bytes.Compare(s.chunk[left].key, s.chunk[right].key) < 0
	})
	if rejectDuplicates {
		for index := 1; index < len(s.chunk); index++ {
			if bytes.Equal(s.chunk[index-1].key, s.chunk[index].key) {
				return ErrSiblingCollision
			}
		}
	}
	writer, err := s.workspace.Create(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = writer.Abort()
		}
	}()
	for _, record := range s.chunk {
		if err := writeSortRecord(writer, record, s.meter); err != nil {
			return err
		}
	}
	object, err := writer.Commit()
	if err != nil {
		return err
	}
	committed = true
	return s.compactRun(ctx, object, rejectDuplicates)
}

// compactRun performs a binary carry as soon as a run is complete. Retaining a
// flat run list makes bookkeeping memory proportional to directory width even
// when record payloads spill correctly; one run per level keeps both the sorter
// and FileSpillWorkspace metadata logarithmically bounded.
func (s *externalSorter) compactRun(ctx context.Context, run SpillObject, rejectDuplicates bool) error {
	for level := 0; ; level++ {
		if level == len(s.runs) {
			s.runs = append(s.runs, run)
			return nil
		}
		if s.runs[level] == nil {
			s.runs[level] = run
			return nil
		}
		merged, err := s.mergePair(ctx, s.runs[level], run, rejectDuplicates)
		if err != nil {
			return err
		}
		s.runs[level] = nil
		run = merged
	}
}

func (s *externalSorter) finish(ctx context.Context, rejectDuplicates bool) (SpillObject, uint64, error) {
	if err := s.flush(ctx, rejectDuplicates); err != nil {
		return nil, 0, err
	}
	if len(s.runs) == 0 {
		writer, err := s.workspace.Create(ctx)
		if err != nil {
			return nil, 0, err
		}
		object, err := writer.Commit()
		if err != nil {
			_ = writer.Abort()
			return nil, 0, err
		}
		s.runs = append(s.runs, object)
	}
	runs := make([]SpillObject, 0, len(s.runs))
	for _, run := range s.runs {
		if run != nil {
			runs = append(runs, run)
		}
	}
	for len(runs) > 1 {
		next := make([]SpillObject, 0, (len(runs)+1)/2)
		for index := 0; index < len(runs); index += 2 {
			if index+1 == len(runs) {
				next = append(next, runs[index])
				continue
			}
			merged, err := s.mergePair(ctx, runs[index], runs[index+1], rejectDuplicates)
			if err != nil {
				return nil, 0, err
			}
			next = append(next, merged)
		}
		runs = next
	}
	return runs[0], s.count, nil
}

func (s *externalSorter) close() {
	if s == nil || s.chunkBudget == nil {
		return
	}
	s.chunkBudget.Release()
	s.chunkBudget = nil
	s.chunk = nil
	s.chunkBytes = 0
}

func (s *externalSorter) mergePair(ctx context.Context, left, right SpillObject, rejectDuplicates bool) (SpillObject, error) {
	leftCursor, err := newSortCursor(ctx, left, s.hierarchy)
	if err != nil {
		return nil, err
	}
	defer leftCursor.close()
	rightCursor, err := newSortCursor(ctx, right, s.hierarchy)
	if err != nil {
		return nil, err
	}
	defer rightCursor.close()
	writer, err := s.workspace.Create(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = writer.Abort()
		}
	}()
	if err := mergeSortRecords(ctx, writer, s.meter, leftCursor, rightCursor, rejectDuplicates); err != nil {
		return nil, err
	}
	object, err := writer.Commit()
	if err != nil {
		return nil, err
	}
	committed = true
	// Windows does not permit unlinking an open run. Close both cursors before
	// retiring their objects; close remains idempotent for the deferred cleanup.
	leftCursor.close()
	rightCursor.close()
	for _, input := range []SpillObject{left, right} {
		size := input.Size()
		if err := input.Remove(); err != nil {
			return nil, err
		}
		if err := s.meter.Release(ResourceUsage{SpillBytes: size}); err != nil {
			return nil, err
		}
	}
	return object, nil
}

func mergeSortRecords(
	ctx context.Context,
	writer io.Writer,
	meter ResourceMeter,
	leftCursor, rightCursor *sortCursor,
	rejectDuplicates bool,
) error {
	leftRecord, leftOK, err := leftCursor.next(ctx)
	if err != nil {
		return err
	}
	rightRecord, rightOK, err := rightCursor.next(ctx)
	if err != nil {
		return err
	}
	for leftOK || rightOK {
		if err := ctx.Err(); err != nil {
			return err
		}
		takeLeft := !rightOK || leftOK && bytes.Compare(leftRecord.key, rightRecord.key) < 0
		if leftOK && rightOK && bytes.Equal(leftRecord.key, rightRecord.key) && rejectDuplicates {
			return ErrSiblingCollision
		}
		if takeLeft {
			err = writeSortRecord(writer, leftRecord, meter)
			if err == nil {
				leftRecord, leftOK, err = leftCursor.next(ctx)
			}
		} else {
			err = writeSortRecord(writer, rightRecord, meter)
			if err == nil {
				rightRecord, rightOK, err = rightCursor.next(ctx)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func writeSortRecord(writer io.Writer, record sortRecord, meter ResourceMeter) error {
	if uint64(len(record.key)) > uint64(^uint32(0)) || uint64(len(record.payload)) > uint64(^uint32(0)) {
		return errors.New("catalog sort record exceeds its framing limit")
	}
	var header [sortRecordHeaderBytes]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(len(record.key)))
	binary.BigEndian.PutUint32(header[4:8], uint32(len(record.payload)))
	charge := ResourceUsage{SpillBytes: sortRecordHeaderBytes + uint64(len(record.key)+len(record.payload))}
	if err := meter.Consume(charge); err != nil {
		return err
	}
	if err := writeFull(writer, header[:]); err != nil {
		return err
	}
	if err := writeFull(writer, record.key); err != nil {
		return err
	}
	if err := writeFull(writer, record.payload); err != nil {
		return err
	}
	return nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return errors.New("catalog writer returned an invalid byte count")
		}
		data = data[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type sortCursor struct {
	reader    io.ReadCloser
	hierarchy BudgetHierarchy
	lease     *BudgetReservation
}

func newSortCursor(ctx context.Context, object SpillObject, hierarchy BudgetHierarchy) (*sortCursor, error) {
	reader, err := object.Open(ctx)
	if err != nil {
		return nil, err
	}
	return &sortCursor{reader: reader, hierarchy: hierarchy}, nil
}

func (c *sortCursor) next(ctx context.Context) (sortRecord, bool, error) {
	if c.lease != nil {
		c.lease.Release()
		c.lease = nil
	}
	if err := ctx.Err(); err != nil {
		return sortRecord{}, false, err
	}
	var header [sortRecordHeaderBytes]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return sortRecord{}, false, nil
		}
		return sortRecord{}, false, fmt.Errorf("read catalog sort record header: %w", err)
	}
	keyBytes := uint64(binary.BigEndian.Uint32(header[0:4]))
	payloadBytes := uint64(binary.BigEndian.Uint32(header[4:8]))
	memory := sortRecordMemoryOverhead + keyBytes + payloadBytes
	lease, err := ReserveHierarchy(c.hierarchy, ResourceUsage{MemoryBytes: memory})
	if err != nil {
		return sortRecord{}, false, err
	}
	c.lease = lease
	record := sortRecord{key: make([]byte, keyBytes), payload: make([]byte, payloadBytes)}
	if _, err := io.ReadFull(c.reader, record.key); err != nil {
		return sortRecord{}, false, fmt.Errorf("read catalog sort key: %w", err)
	}
	if _, err := io.ReadFull(c.reader, record.payload); err != nil {
		return sortRecord{}, false, fmt.Errorf("read catalog sort payload: %w", err)
	}
	return record, true, nil
}

func (c *sortCursor) close() {
	if c.lease != nil {
		c.lease.Release()
		c.lease = nil
	}
	_ = c.reader.Close()
}

type sortedNodeSource struct {
	object    SpillObject
	count     uint64
	hierarchy BudgetHierarchy
}

func (s sortedNodeSource) Count() uint64 { return s.count }

func (s sortedNodeSource) Open(ctx context.Context) (NodeRecordIterator, error) {
	cursor, err := newSortCursor(ctx, s.object, s.hierarchy)
	if err != nil {
		return nil, err
	}
	return &sortedNodeIterator{cursor: cursor}, nil
}

func (s sortedNodeSource) Release(meter ResourceMeter) error {
	size := s.object.Size()
	if err := s.object.Remove(); err != nil {
		return err
	}
	return meter.Release(ResourceUsage{SpillBytes: size})
}

type sortedNodeIterator struct {
	cursor *sortCursor
}

func (i *sortedNodeIterator) Next(ctx context.Context) (NodeRecord, bool, error) {
	record, ok, err := i.cursor.next(ctx)
	if err != nil || !ok {
		return NodeRecord{}, ok, err
	}
	node, err := decodeNodeRecord(record.payload)
	if err != nil {
		return NodeRecord{}, false, err
	}
	if !bytes.Equal(record.key, []byte(node.Entry().Name())) {
		return NodeRecord{}, false, fmt.Errorf("%w: sorted node key changed", ErrCorruptCatalogStorage)
	}
	return node, true, nil
}

func (i *sortedNodeIterator) Close() error {
	i.cursor.close()
	return nil
}

type directorySorter struct {
	parent     DirectoryID
	nodes      *externalSorter
	validation *externalSorter
	meter      ResourceMeter
	hierarchy  BudgetHierarchy
}

func newDirectorySorter(parent DirectoryID, workspace SpillWorkspace, meter ResourceMeter, hierarchy BudgetHierarchy, runBytes uint64) (*directorySorter, error) {
	nodes, err := newExternalSorter(workspace, meter, hierarchy, runBytes)
	if err != nil {
		return nil, err
	}
	validation, err := newExternalSorter(workspace, meter, hierarchy, runBytes)
	if err != nil {
		return nil, err
	}
	return &directorySorter{parent: parent, nodes: nodes, validation: validation, meter: meter, hierarchy: hierarchy}, nil
}

func (s *directorySorter) Add(ctx context.Context, child ScannedChild) error {
	node, err := child.nodeRecord(s.parent)
	if err != nil {
		return err
	}
	encodingMemory, err := ReserveHierarchy(
		s.hierarchy, ResourceUsage{MemoryBytes: node.EstimatedMemoryBytes()},
	)
	if err != nil {
		return err
	}
	defer encodingMemory.Release()
	encoded, err := encodeNodeRecord(node)
	if err != nil {
		return err
	}
	if err := s.nodes.add(ctx, sortRecord{key: []byte(node.Entry().Name()), payload: encoded}); err != nil {
		return err
	}
	nameKey := append([]byte{0}, []byte(siblingCollisionKey(node.Entry().Name()))...)
	if err := s.validation.add(ctx, sortRecord{key: nameKey}); err != nil {
		return err
	}
	nodeKey := append([]byte{1}, node.NodeID().Bytes()...)
	return s.validation.add(ctx, sortRecord{key: nodeKey})
}

func (s *directorySorter) Finish(ctx context.Context) (sortedNodeSource, error) {
	nodes, count, err := s.nodes.finish(ctx, true)
	if err != nil {
		return sortedNodeSource{}, err
	}
	validation, _, err := s.validation.finish(ctx, true)
	if err != nil {
		_ = nodes.Remove()
		_ = s.meter.Release(ResourceUsage{SpillBytes: nodes.Size()})
		return sortedNodeSource{}, err
	}
	validationBytes := validation.Size()
	if err := validation.Remove(); err != nil {
		return sortedNodeSource{}, err
	}
	if err := s.meter.Release(ResourceUsage{SpillBytes: validationBytes}); err != nil {
		return sortedNodeSource{}, err
	}
	return sortedNodeSource{object: nodes, count: count, hierarchy: s.hierarchy}, nil
}

func (s *directorySorter) Close() {
	if s == nil {
		return
	}
	s.nodes.close()
	s.validation.close()
}
