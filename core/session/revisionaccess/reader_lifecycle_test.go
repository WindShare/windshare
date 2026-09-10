package revisionaccess

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func TestInFlightBlockCanObserveDetachedLeaseBeforeRenewalReply(t *testing.T) {
	reader, source, opened := fixture(t)
	source.read = func(ctx context.Context, id content.LeaseID, requested content.Range, sink transfer.RangeSink) error {
		if id[0] == 1 {
			return content.ErrInvalidLease
		}
		return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.End-requested.Offset))
	}
	if err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil })); err != nil {
		t.Fatal(err)
	}
	if source.opens != 2 {
		t.Fatal(source.opens)
	}
}

func TestLateOrMalformedOpenCannotLeakWireLease(t *testing.T) {
	for _, cause := range []string{"descriptor", "file", "zero lease", "caller canceled", "owner closed"} {
		t.Run(cause, func(t *testing.T) {
			reader, source, opened := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source.open = func(context.Context, int) (Lease, error) {
				lease := Lease{LeaseID: identity[content.LeaseID](2), Descriptor: opened.Descriptor}
				switch cause {
				case "descriptor":
					lease.Descriptor = content.FileRevisionDescriptor{}
				case "file":
					d := opened.Descriptor
					file := d.FileID()
					file[0]++
					lease.Descriptor, _ = content.NewFileRevisionDescriptor(d.ShareInstance(), file, d.FileRevision(), d.Geometry(), d.ModifiedTime())
				case "zero lease":
					lease.LeaseID = content.LeaseID{}
				case "caller canceled":
					cancel()
				case "owner closed":
					reader.Stop()
				}
				return lease, nil
			}
			if _, err := reader.OpenRevision(ctx, opened.Descriptor.FileID()); err == nil {
				t.Fatal("invalid open accepted")
			}
			if cause != "zero lease" && (len(source.released) != 1 || source.released[0][0] != 2) {
				t.Fatal(source.released)
			}
		})
	}
}

func TestReplacementCleanupFailureIsReturnedWithCurrentLeaseStillOwned(t *testing.T) {
	reader, source, opened := fixture(t)
	failure := errors.New("release transport failure")
	source.release = func(id content.LeaseID) error {
		if id[0] == 1 {
			return failure
		}
		return nil
	}
	source.read = func(context.Context, content.LeaseID, content.Range, transfer.RangeSink) error {
		return content.ErrLeaseExpired
	}
	err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }))
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := reader.ReleaseRevision(context.Background(), opened.Handle); err != nil {
		t.Fatal(err)
	}
	if len(source.released) != 2 || source.released[1][0] != 2 {
		t.Fatal(source.released)
	}
}

func TestMalformedReplacementCannotAliasOriginalLease(t *testing.T) {
	for _, id := range []content.LeaseID{{}, identity[content.LeaseID](1)} {
		reader, source, opened := fixture(t)
		source.open = func(context.Context, int) (Lease, error) {
			return Lease{LeaseID: id, Descriptor: opened.Descriptor}, nil
		}
		source.read = func(context.Context, content.LeaseID, content.Range, transfer.RangeSink) error {
			return content.ErrLeaseExpired
		}
		err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }))
		if !errors.Is(err, transfer.ErrRevisionIdentity) || len(source.released) != 0 {
			t.Fatalf("err=%v releases=%v", err, source.released)
		}
	}
}

func TestStopCancelsReadersAndQueuedOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, source, opened := fixture(t)
		entered := make(chan struct{})
		source.read = func(ctx context.Context, _ content.LeaseID, _ content.Range, _ transfer.RangeSink) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		sink := transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil })
		results := make(chan error, 2)
		go func() {
			results <- reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink)
		}()
		<-entered
		go func() {
			results <- reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, sink)
		}()
		synctest.Wait()
		reader.Stop()
		for range 2 {
			if err := <-results; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		}
	})
}

func TestBrokenSourceRangesCannotInventProgress(t *testing.T) {
	for _, position := range []struct {
		offset uint64
		data   []byte
	}{{1, []byte{1}}, {0, nil}, {0, make([]byte, 7)}} {
		reader, source, opened := fixture(t)
		source.read = func(ctx context.Context, _ content.LeaseID, _ content.Range, sink transfer.RangeSink) error {
			return sink.WriteRange(ctx, position.offset, position.data)
		}
		err := reader.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{End: 6}, transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { t.Fatal("invalid source reached output"); return nil }))
		if !errors.Is(err, transfer.ErrBlockIdentity) {
			t.Fatal(err)
		}
	}
}
