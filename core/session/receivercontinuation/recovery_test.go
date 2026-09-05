package receivercontinuation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

func TestInterruptedRangeRetainsOnlyAuthenticatedDeliveredIntervals(t *testing.T) {
	f := newFixture(t)
	runtime, channel := f.connect()
	var count atomic.Int32
	s, _ := New(context.Background(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		count.Add(1)
		next, _ := f.connect()
		return next, nil
	})
	defer s.Close()
	snapshot, release, err := s.AcquireDirectory(context.Background(), runtime.Descriptor().SyntheticRoot())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	entries := snapshot.Pages()[0].Entries()
	if len(entries) != 1 {
		t.Fatalf("entries=%v", entries)
	}
	file, isFile := entries[0].FileID()
	if !isFile {
		t.Fatal("source is not file")
	}
	opened, err := s.OpenRevision(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	written := make([]byte, len(f.payload))
	covered := make([]bool, len(f.payload))
	var mu sync.Mutex
	var cut sync.Once
	sink := transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		for index := range data {
			position := int(offset) + index
			if covered[position] {
				t.Errorf("duplicate useful byte at %d", position)
			}
			covered[position] = true
			written[position] = data[index]
		}
		cut.Do(func() { _ = channel.Close() })
		return nil
	})
	err = s.ReadRange(context.Background(), opened.LeaseID, opened.Descriptor, content.Range{End: opened.Descriptor.ExactSize()}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 || !bytes.Equal(written, f.payload) {
		t.Fatalf("replacement=%d output=%d", count.Load(), len(written))
	}
	if err = s.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseRevision(context.Background(), opened.LeaseID); !errors.Is(err, content.ErrInvalidLease) {
		t.Fatal(err)
	}
	if err = s.ReadRange(context.Background(), opened.LeaseID, opened.Descriptor, content.Range{End: 1}, sink); !errors.Is(err, content.ErrInvalidLease) {
		t.Fatal(err)
	}
}

func TestReplacementRevisionDriftRejectsRetainedProgress(t *testing.T) {
	f := newFixture(t, true)
	runtime, wire := f.connect()
	s, err := New(context.Background(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		wire.sender.Close()
		replacement := f.filename + ".replacement"
		if writeErr := os.WriteFile(replacement, []byte("changed revision after network loss"), 0600); writeErr != nil {
			return nil, writeErr
		}
		if renameErr := os.Rename(replacement, f.filename); renameErr != nil {
			return nil, renameErr
		}
		next, _ := f.connect()
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snapshot, release, err := s.AcquireDirectory(context.Background(), runtime.Descriptor().SyntheticRoot())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	directory, _ := snapshot.Pages()[0].Entries()[0].DirectoryID()
	children, releaseChildren, err := s.AcquireDirectory(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseChildren()
	file, _ := children.Pages()[0].Entries()[0].FileID()
	opened, err := s.OpenRevision(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	first := content.Range{End: 1024}
	var delivered uint64
	sink := transfer.RangeSinkFunc(func(_ context.Context, _ uint64, data []byte) error { delivered += uint64(len(data)); return nil })
	if err = s.ReadRange(context.Background(), opened.LeaseID, opened.Descriptor, first, sink); err != nil {
		t.Fatal(err)
	}
	dependencies, _ := runtime.TransferDependencies()
	if err = dependencies.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
		t.Fatal(err)
	}
	_ = wire.Close()
	<-runtime.Done()
	err = s.ReadRange(context.Background(), opened.LeaseID, opened.Descriptor, content.Range{Offset: first.End, End: opened.Descriptor.ExactSize()}, sink)
	if !errors.Is(err, content.ErrRevisionStale) || delivered != first.End {
		t.Fatalf("drift=%v delivered=%d", err, delivered)
	}
	if err = s.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
		t.Fatal(err)
	}
}

func TestFreshLeaseCannotCertifyDifferentRetainedRevision(t *testing.T) {
	f := newFixture(t)
	previous, _ := f.connect()
	next, _ := f.connect()
	dependencies, _ := previous.TransferDependencies()
	snapshot, release, err := dependencies.AcquireDirectory(context.Background(), previous.Descriptor().SyntheticRoot())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	file, _ := snapshot.Pages()[0].Entries()[0].FileID()
	opened, err := dependencies.OpenRevision(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dependencies.ReleaseRevision(context.Background(), opened.LeaseID) }()
	changedRevision := opened.Descriptor.FileRevision()
	changedRevision[0] ^= 1
	changed, err := content.NewFileRevisionDescriptor(opened.Descriptor.ShareInstance(), file, changedRevision, opened.Descriptor.Geometry(), opened.Descriptor.ModifiedTime())
	if err != nil {
		t.Fatal(err)
	}
	binding := &revisionLease{runtime: previous, opened: opened}
	if err = binding.refresh(context.Background(), next, changed); !errors.Is(err, content.ErrRevisionDrift) {
		t.Fatalf("fresh lease accepted changed immutable revision: %v", err)
	}
	if binding.runtime != previous || binding.opened != opened {
		t.Fatal("rejected lease replaced retained binding")
	}
}

func TestConcurrentRecoveryJoinsOneFlightAndRejectsReusedSession(t *testing.T) {
	f := newFixture(t)
	runtime, channel := f.connect()
	entered, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	s, _ := New(context.Background(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		count.Add(1)
		close(entered)
		<-release
		next, _ := f.connect()
		return next, nil
	})
	defer s.Close()
	_ = channel.Close()
	<-runtime.Done()
	results := make(chan error, 20)
	for range 20 {
		go func() { results <- s.recover(context.Background(), runtime) }()
	}
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.recover(canceled, runtime); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	for range 20 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 1 {
		t.Fatalf("replacement flights=%d", count.Load())
	}
	if err := s.recover(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	bad, wire := f.connect()
	rejected, _ := New(context.Background(), bad, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		return bad, nil
	})
	defer rejected.Close()
	_ = wire.Close()
	<-bad.Done()
	if err := rejected.recover(context.Background(), bad); !errors.Is(err, ErrReplacement) {
		t.Fatal(err)
	}
	if err := rejected.recover(context.Background(), bad); !errors.Is(err, ErrReplacement) {
		t.Fatal(err)
	}
}

type failedCursor struct{}

func (failedCursor) Next(context.Context) (catalog.CatalogPage, bool, error) {
	return catalog.CatalogPage{}, false, sessionruntime.ErrRuntimeClosed
}
func (failedCursor) Close() error { return nil }

func TestCatalogReplayPreservesCommitmentAndGenerationFence(t *testing.T) {
	f := newFixture(t)
	runtime, wire := f.connect()
	s, _ := New(context.Background(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		next, _ := f.connect()
		return next, nil
	})
	defer s.Close()
	cursor, err := s.OpenDirectoryPages(context.Background(), runtime.Descriptor().SyntheticRoot())
	if err != nil {
		t.Fatal(err)
	}
	first, exists, err := cursor.Next(context.Background())
	if err != nil || !exists {
		t.Fatal(err)
	}
	original := s.Current()
	c := cursor.(*directoryCursor)
	_ = c.cursor.Close()
	c.cursor = failedCursor{}
	_ = wire.Close()
	<-runtime.Done()
	if _, exists, err = cursor.Next(context.Background()); err != nil || exists {
		t.Fatalf("replayed end exists=%v err=%v", exists, err)
	}
	if c.commitment != first.Commitment() {
		t.Fatal("recovery changed prior page")
	}
	if err = cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if err = cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = cursor.Next(context.Background()); !errors.Is(err, sessionruntime.ErrRuntimeClosed) {
		t.Fatal(err)
	}
	change, err := s.WaitForChange(context.Background(), original)
	if err != nil || change.Kind() != revisionwait.GenerationReplaced {
		t.Fatalf("change=%v err=%v", change, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.WaitForChange(canceled, s.Current()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	rules, _ := transfer.NewSelectionRules(true, nil)
	selection, _ := transfer.NewSelectionSpec(runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules)
	if _, err = s.ResolveOrdinaryOutputShape(context.Background(), selection, ordinaryoutput.DefaultShapeProbeBudgetV1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveOrdinaryOutputShape(context.Background(), transfer.SelectionSpec{}, ordinaryoutput.DefaultShapeProbeBudgetV1, nil); err == nil {
		t.Fatal("invalid selection accepted")
	}
	if _, err = s.NewTransferJob(transfer.ReceiveIntent{}, transfer.TransferJobID{}, nil, nil); !errors.Is(err, transfer.ErrInvalidTransferJob) {
		t.Fatal(err)
	}
}

func TestRemainingIntervalsDoNotRetainPayloadOrConsumeFailedWrites(t *testing.T) {
	failure := errors.New("output paused")
	sink := &remainingSink{target: transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return failure }), missing: []content.Range{{Offset: 3, End: 20}}}
	if err := sink.WriteRange(context.Background(), 5, []byte("abc")); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if got := sink.ranges(); len(got) != 1 || got[0].Offset != 3 || got[0].End != 20 {
		t.Fatal(got)
	}
	sink.target = transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil })
	_ = sink.WriteRange(context.Background(), 6, []byte("abc"))
	_ = sink.WriteRange(context.Background(), 3, []byte("abc"))
	_ = sink.WriteRange(context.Background(), 12, []byte("abcdefgh"))
	got := sink.ranges()
	if len(got) != 1 || got[0] != (content.Range{Offset: 9, End: 12}) {
		t.Fatal(got)
	}
}

func TestFenceRecoveryCancellationJoinsReplacementOwner(t *testing.T) {
	f := newFixture(t)
	runtime, wire := f.connect()
	entered := make(chan struct{})
	s, _ := New(context.Background(), runtime, func(ctx context.Context, _ *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	expected := s.Current()
	_ = wire.Close()
	<-runtime.Done()
	result := make(chan error, 1)
	go func() { _, err := s.WaitForChange(context.Background(), expected); result <- err }()
	<-entered
	s.Close()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement did not join")
	}
}
