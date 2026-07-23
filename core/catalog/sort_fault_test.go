package catalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
	"testing"
)

type fakeSpillWorkspace struct {
	createErr error
	writer    *fakeSpillWriter
	closed    bool
}

func (w *fakeSpillWorkspace) Create(context.Context) (SpillWriter, error) {
	if w.createErr != nil {
		return nil, w.createErr
	}
	if w.writer == nil {
		w.writer = &fakeSpillWriter{}
	}
	return w.writer, nil
}
func (w *fakeSpillWorkspace) Close() error {
	w.closed = true
	return nil
}

type fakeSpillWriter struct {
	data      bytes.Buffer
	writeErr  error
	commitErr error
	aborted   bool
}

type boundedWriter struct {
	buffer bytes.Buffer
	limit  int
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if len(data) > w.limit {
		data = data[:w.limit]
	}
	return w.buffer.Write(data)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func (w *fakeSpillWriter) Write(data []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.data.Write(data)
}
func (w *fakeSpillWriter) Commit() (SpillObject, error) {
	if w.commitErr != nil {
		return nil, w.commitErr
	}
	owned := append([]byte(nil), w.data.Bytes()...)
	return &fakeSpillObject{data: owned, size: uint64(len(owned))}, nil
}
func (w *fakeSpillWriter) Abort() error {
	w.aborted = true
	return nil
}

type fakeSpillObject struct {
	data      []byte
	size      uint64
	openErr   error
	removeErr error
	removed   bool
}

func (o *fakeSpillObject) Open(context.Context) (io.ReadCloser, error) {
	if o.openErr != nil {
		return nil, o.openErr
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}
func (o *fakeSpillObject) Size() uint64 { return o.size }
func (o *fakeSpillObject) Remove() error {
	o.removed = true
	return o.removeErr
}

func sorterBudget(t *testing.T, memory, spill uint64) (BudgetHierarchy, *attemptResourceMeter) {
	t.Helper()
	limits := BudgetLimits{
		ActiveScans: 1, ScanWork: 100, Entries: 100, MemoryBytes: memory, SpillBytes: spill,
	}
	process, _ := NewBudgetAccount("process", limits)
	share, _ := NewBudgetAccount("share", limits)
	session, _ := NewBudgetAccount("session", limits)
	hierarchy := BudgetHierarchy{Process: process, Share: share, Session: session}
	meter, err := newAttemptResourceMeter(hierarchy)
	if err != nil {
		t.Fatal(err)
	}
	return hierarchy, meter
}

func TestExternalSorterFaultMatrix(t *testing.T) {
	injected := errors.New("injected")
	for name, workspace := range map[string]*fakeSpillWorkspace{
		"create": {createErr: injected},
		"write":  {writer: &fakeSpillWriter{writeErr: injected}},
		"commit": {writer: &fakeSpillWriter{commitErr: injected}},
	} {
		t.Run(name, func(t *testing.T) {
			hierarchy, meter := sorterBudget(t, 1<<20, 1<<20)
			defer meter.Close()
			sorter, err := newExternalSorter(workspace, meter, hierarchy, 1024)
			if err != nil {
				t.Fatal(err)
			}
			if err := sorter.add(context.Background(), sortRecord{key: []byte("key"), payload: []byte("value")}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := sorter.finish(context.Background(), true); !errors.Is(err, injected) {
				t.Fatalf("sort fault = %v", err)
			}
			sorter.close()
		})
	}

	hierarchy, meter := sorterBudget(t, 1<<20, 1<<20)
	defer meter.Close()
	workspace := &fakeSpillWorkspace{}
	sorter, _ := newExternalSorter(workspace, meter, hierarchy, 1024)
	if err := sorter.add(context.Background(), sortRecord{key: []byte("same")}); err != nil {
		t.Fatal(err)
	}
	if err := sorter.add(context.Background(), sortRecord{key: []byte("same")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sorter.finish(context.Background(), true); !errors.Is(err, ErrSiblingCollision) {
		t.Fatalf("duplicate sort key = %v", err)
	}
	sorter.close()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	sorter, _ = newExternalSorter(&fakeSpillWorkspace{}, meter, hierarchy, 1024)
	if err := sorter.add(cancelled, sortRecord{key: []byte("key")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled add = %v", err)
	}
	sorter.runs = []SpillObject{
		&fakeSpillObject{openErr: injected},
		&fakeSpillObject{data: nil},
	}
	if _, _, err := sorter.finish(context.Background(), true); !errors.Is(err, injected) {
		t.Fatalf("merge open fault = %v", err)
	}
}

func TestSortCursorRejectsTruncationAndBudgetOverflow(t *testing.T) {
	injected := errors.New("open")
	hierarchy, meter := sorterBudget(t, 1<<20, 1<<20)
	defer meter.Close()
	if _, err := newSortCursor(context.Background(), &fakeSpillObject{openErr: injected}, hierarchy); !errors.Is(err, injected) {
		t.Fatalf("cursor open fault = %v", err)
	}
	for name, data := range map[string][]byte{
		"header":  {0},
		"key":     framedSortBytes(2, 0, []byte{1}),
		"payload": framedSortBytes(0, 2, []byte{1}),
	} {
		t.Run(name, func(t *testing.T) {
			cursor, err := newSortCursor(context.Background(), &fakeSpillObject{data: data}, hierarchy)
			if err != nil {
				t.Fatal(err)
			}
			defer cursor.close()
			if _, _, err := cursor.next(context.Background()); err == nil {
				t.Fatal("truncated sort record was accepted")
			}
		})
	}
	cursor, err := newSortCursor(context.Background(), &fakeSpillObject{}, hierarchy)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := cursor.next(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cursor = %v", err)
	}
	cursor.close()

	tinyHierarchy, tinyMeter := sorterBudget(t, 1, 1024)
	defer tinyMeter.Close()
	cursor, err = newSortCursor(context.Background(), &fakeSpillObject{data: framedSortBytes(2, 0, []byte{1, 2})}, tinyHierarchy)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cursor.next(context.Background()); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("cursor memory budget = %v", err)
	}
	cursor.close()
}

func framedSortBytes(keyBytes, payloadBytes uint32, body []byte) []byte {
	var header [8]byte
	binary.BigEndian.PutUint32(header[0:4], keyBytes)
	binary.BigEndian.PutUint32(header[4:8], payloadBytes)
	return append(header[:], body...)
}

func TestSortedSourceAndWriterPropagateStorageFailures(t *testing.T) {
	injected := errors.New("storage")
	hierarchy, meter := sorterBudget(t, 1<<20, 1<<20)
	defer meter.Close()
	source := sortedNodeSource{object: &fakeSpillObject{openErr: injected}, count: 1, hierarchy: hierarchy}
	if _, err := source.Open(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("source open = %v", err)
	}
	source.object = &fakeSpillObject{removeErr: injected, size: 1}
	if err := source.Release(meter); !errors.Is(err, injected) {
		t.Fatalf("source remove = %v", err)
	}
	limitedHierarchy, limitedMeter := sorterBudget(t, 1024, 1)
	defer limitedMeter.Close()
	if err := writeSortRecord(io.Discard, sortRecord{key: []byte("too-large")}, limitedMeter); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("sort spill budget = %v", err)
	}
	_ = limitedHierarchy
}

func TestSortRecordFramingHandlesPartialWriters(t *testing.T) {
	_, meter := sorterBudget(t, 1<<20, 1<<20)
	defer meter.Close()
	record := sortRecord{key: []byte("key"), payload: []byte("payload")}
	writer := &boundedWriter{limit: 2}
	if err := writeSortRecord(writer, record, meter); err != nil {
		t.Fatal(err)
	}
	want := framedSortBytes(uint32(len(record.key)), uint32(len(record.payload)), append(bytes.Clone(record.key), record.payload...))
	if !bytes.Equal(writer.buffer.Bytes(), want) {
		t.Fatal("partial writes changed sort framing")
	}
	if err := writeFull(zeroWriter{}, []byte("blocked")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-progress writer = %v", err)
	}
}

func TestExternalSorterCompactsRunMetadataWhileStreaming(t *testing.T) {
	const recordCount = 2_048
	hierarchy, meter := sorterBudget(t, 1<<20, 64<<20)
	defer meter.Close()
	factory := NewFileSpillFactory(t.TempDir())
	workspaceValue, err := factory.NewWorkspace(context.Background(), SpillRequest{
		ShareInstance: idValue[ShareInstance](1), AttemptID: idValue[ScanAttemptID](2),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceValue.Close()
	workspace := workspaceValue.(*fileSpillWorkspace)
	sorter, err := newExternalSorter(workspace, meter, hierarchy, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sorter.close()
	var key [4]byte
	for index := range recordCount {
		binary.BigEndian.PutUint32(key[:], uint32(recordCount-index))
		if err := sorter.add(context.Background(), sortRecord{key: bytes.Clone(key[:])}); err != nil {
			t.Fatal(err)
		}
		workspace.mu.Lock()
		liveObjects := len(workspace.objects)
		workspace.mu.Unlock()
		maxLive := bits.Len(uint(index+1)) + 1
		if liveObjects > maxLive || len(sorter.runs) > maxLive {
			t.Fatalf("run metadata grew with width: live=%d levels=%d max=%d", liveObjects, len(sorter.runs), maxLive)
		}
	}
	object, count, err := sorter.finish(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if count != recordCount {
		t.Fatalf("sorted count = %d", count)
	}
	objectBytes := object.Size()
	if err := object.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := meter.Release(ResourceUsage{SpillBytes: objectBytes}); err != nil {
		t.Fatal(err)
	}
}
