package revisionaccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

type sourceFixture struct {
	opens      int
	released   []content.LeaseID
	descriptor content.FileRevisionDescriptor
	open       func(context.Context, int) (Lease, error)
	read       func(context.Context, content.LeaseID, content.Range, transfer.RangeSink) error
	release    func(content.LeaseID) error
}

func (s *sourceFixture) OpenLease(ctx context.Context, _ catalog.FileID) (Lease, error) {
	s.opens++
	if s.open != nil {
		return s.open(ctx, s.opens)
	}
	return NewLease(identity[content.LeaseID](byte(s.opens)), s.descriptor)
}
func (s *sourceFixture) ReleaseLease(_ context.Context, id content.LeaseID) error {
	s.released = append(s.released, id)
	if s.release != nil {
		return s.release(id)
	}
	return nil
}
func (s *sourceFixture) ReadLeaseRange(ctx context.Context, id content.LeaseID, _ content.FileRevisionDescriptor, requested content.Range, sink transfer.RangeSink) error {
	return s.read(ctx, id, requested, sink)
}
func identity[T ~[16]byte](value byte) T { var id T; id[0] = value; return id }

func fixture(t *testing.T) (*Reader, *sourceFixture, transfer.OpenedRevision) {
	t.Helper()
	geometry, _ := content.NewFileGeometry(6, catalog.MinChunkSize)
	descriptor, _ := content.NewFileRevisionDescriptor(identity[catalog.ShareInstance](11), identity[catalog.FileID](12), identity[content.FileRevision](13), geometry, catalog.ModifiedTime{})
	source := &sourceFixture{descriptor: descriptor}
	reader := New(context.Background(), source)
	t.Cleanup(reader.Stop)
	opened, err := reader.OpenRevision(context.Background(), descriptor.FileID())
	if err != nil {
		t.Fatal(err)
	}
	return reader, source, opened
}

func TestExpiredLeasesPreserveDeliveredBytesAcrossMultipleReplacements(t *testing.T) {
	reader, source, opened := fixture(t)
	source.read = func(ctx context.Context, lease content.LeaseID, requested content.Range, sink transfer.RangeSink) error {
		wantOffset := uint64(lease[0]-1) * 2
		if requested.Offset != wantOffset {
			t.Fatalf("lease %v reread range %v", lease, requested)
		}
		if err := sink.WriteRange(ctx, requested.Offset, []byte{lease[0], lease[0]}); err != nil {
			return err
		}
		if lease[0] < 3 {
			return fmt.Errorf("authenticated expiry: %w", content.ErrLeaseExpired)
		}
		return nil
	}
	var got []byte
	sink := transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
		if offset != uint64(len(got)) {
			t.Fatalf("duplicate or skipped output at %d", offset)
		}
		got = append(got, data...)
		return nil
	})
	if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{1, 1, 2, 2, 3, 3}) || source.opens != 3 {
		t.Fatalf("bytes=%v opens=%d", got, source.opens)
	}
	if err := reader.ReleaseRevision(context.Background(), opened.Handle); err != nil {
		t.Fatal(err)
	}
	if len(source.released) != 3 || source.released[2][0] != 3 {
		t.Fatalf("lease ownership leaked: %v", source.released)
	}
}

func TestReplacementChecksImmutableIdentityBeforeWriting(t *testing.T) {
	for _, change := range []string{"revision", "geometry", "file", "share"} {
		t.Run(change, func(t *testing.T) {
			reader, source, opened := fixture(t)
			source.read = func(context.Context, content.LeaseID, content.Range, transfer.RangeSink) error {
				return content.ErrLeaseExpired
			}
			source.open = func(_ context.Context, _ int) (Lease, error) {
				d := opened.Descriptor
				share, file, revision, geometry := d.ShareInstance(), d.FileID(), d.FileRevision(), d.Geometry()
				switch change {
				case "revision":
					revision[0]++
				case "geometry":
					geometry, _ = content.NewFileGeometry(7, catalog.MinChunkSize)
				case "file":
					file[0]++
				case "share":
					share[0]++
				}
				changed, _ := content.NewFileRevisionDescriptor(share, file, revision, geometry, d.ModifiedTime())
				return NewLease(identity[content.LeaseID](2), changed)
			}
			sink := transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { t.Fatal("mixed versions"); return nil })
			for range 2 {
				if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink); !errors.Is(err, content.ErrRevisionDrift) {
					t.Fatal(err)
				}
			}
			if source.opens != 2 || len(source.released) != 1 || source.released[0][0] != 2 {
				t.Fatalf("replacement not compensated: %+v", source)
			}
			if err := reader.ReleaseRevision(context.Background(), opened.Handle); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentReadersShareReplacementAndCancellationDoesNotWaitForOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, source, opened := fixture(t)
		entered, proceed := make(chan struct{}), make(chan struct{})
		source.open = func(ctx context.Context, count int) (Lease, error) {
			close(entered)
			select {
			case <-ctx.Done():
				return Lease{}, ctx.Err()
			case <-proceed:
			}
			return NewLease(identity[content.LeaseID](byte(count)), source.descriptor)
		}
		source.read = func(ctx context.Context, id content.LeaseID, requested content.Range, sink transfer.RangeSink) error {
			if id[0] == 1 {
				return content.ErrLeaseExpired
			}
			return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.End-requested.Offset))
		}
		sink := transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil })
		read := func(ctx context.Context) error {
			return reader.ReadRange(ctx, opened.Handle, opened.Descriptor, content.Range{End: 6}, sink)
		}
		results := make(chan error, 8)
		go func() { results <- read(context.Background()) }()
		<-entered
		for range 7 {
			go func() { results <- read(context.Background()) }()
		}
		ctx, cancel := context.WithCancel(context.Background())
		canceled := make(chan error, 1)
		go func() { canceled <- read(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-canceled; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(proceed)
		for range 8 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if source.opens != 2 {
			t.Fatalf("concurrent replacement opens=%d", source.opens)
		}
		if err := reader.ReleaseRevision(context.Background(), opened.Handle); err != nil {
			t.Fatal(err)
		}
	})
}

func TestExpiredFreshLeaseWithoutProgressStopsReopening(t *testing.T) {
	reader, source, opened := fixture(t)
	source.read = func(context.Context, content.LeaseID, content.Range, transfer.RangeSink) error {
		return content.ErrLeaseExpired
	}
	err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }))
	if !errors.Is(err, content.ErrLeaseExpired) || source.opens != 2 {
		t.Fatalf("error=%v opens=%d", err, source.opens)
	}
}

func TestCapacityWaitRetainsHandleAndRetriesOnlyMissingRange(t *testing.T) {
	reader, source, opened := fixture(t)
	capacity := errors.New("authenticated capacity signal")
	source.open = func(_ context.Context, count int) (Lease, error) {
		if count == 2 {
			return Lease{}, capacity
		}
		return NewLease(identity[content.LeaseID](3), source.descriptor)
	}
	source.read = func(ctx context.Context, id content.LeaseID, requested content.Range, sink transfer.RangeSink) error {
		if id[0] == 1 {
			return content.ErrLeaseExpired
		}
		return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.End-requested.Offset))
	}
	sink := transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil })
	if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink); err != capacity {
		t.Fatal(err)
	}
	if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink); err != nil {
		t.Fatal(err)
	}
	if source.opens != 3 {
		t.Fatal(source.opens)
	}
}

func TestOutputAndSourceFailuresCannotAuthorizeLeaseReplacement(t *testing.T) {
	for _, failure := range []error{content.ErrRevisionDrift, content.ErrSourceDrift, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			reader, source, opened := fixture(t)
			source.read = func(context.Context, content.LeaseID, content.Range, transfer.RangeSink) error { return failure }
			err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }))
			if err != failure || source.opens != 1 {
				t.Fatalf("err=%v opens=%d", err, source.opens)
			}
		})
	}
	reader, source, opened := fixture(t)
	source.read = func(ctx context.Context, _ content.LeaseID, _ content.Range, sink transfer.RangeSink) error {
		return sink.WriteRange(ctx, 0, []byte{1})
	}
	err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return content.ErrLeaseExpired }))
	if !errors.Is(err, content.ErrLeaseExpired) || source.opens != 1 {
		t.Fatalf("output error reopened lease: %v", err)
	}
}

func TestReleaseCancelsInFlightReplacementAndCompensatesLateOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, source, opened := fixture(t)
		entered := make(chan struct{})
		source.open = func(ctx context.Context, _ int) (Lease, error) {
			close(entered)
			<-ctx.Done()
			return NewLease(identity[content.LeaseID](2), source.descriptor)
		}
		source.read = func(context.Context, content.LeaseID, content.Range, transfer.RangeSink) error {
			return content.ErrLeaseExpired
		}
		done := make(chan error, 1)
		go func() {
			done <- reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }))
		}()
		<-entered
		if err := reader.ReleaseRevision(context.Background(), opened.Handle); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(source.released) != 2 || source.released[0][0] != 2 || source.released[1][0] != 1 {
			t.Fatal(source.released)
		}
	})
}

func TestReaderBoundariesAndCleanup(t *testing.T) {
	reader, source, opened := fixture(t)
	sink := transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil })
	source.read = func(context.Context, content.LeaseID, content.Range, transfer.RangeSink) error { return nil }
	if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink); !errors.Is(err, transfer.ErrBlockIdentity) {
		t.Fatal(err)
	}
	for _, requested := range []content.Range{{}, {End: 7}, {Offset: 3, End: 2}} {
		if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, requested, sink); !errors.Is(err, transfer.ErrInvalidDemand) {
			t.Fatal(err)
		}
	}
	if err := reader.ReadRange(context.Background(), opened.Handle, content.FileRevisionDescriptor{}, content.Range{End: 6}, sink); !errors.Is(err, transfer.ErrBlockIdentity) {
		t.Fatal(err)
	}
	source.release = func(content.LeaseID) error { return content.ErrInvalidLease }
	if err := reader.ReleaseRevision(context.Background(), opened.Handle); err != nil {
		t.Fatal(err)
	}
	if err := reader.ReleaseRevision(context.Background(), opened.Handle); !errors.Is(err, content.ErrInvalidLease) {
		t.Fatal(err)
	}
	if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink); !errors.Is(err, content.ErrInvalidLease) {
		t.Fatal(err)
	}
	reader.Stop()
	if _, err := reader.OpenRevision(context.Background(), source.descriptor.FileID()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := reader.ReleaseRevision(context.Background(), opened.Handle); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
