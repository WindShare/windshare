package receivercontinuation

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

func TestLongDownloadCrossesLeaseLifetimesWithoutReplacingSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		runtime, _ := f.connect()
		sessionID := runtime.ProtocolSessionID()
		continuation, err := New(context.Background(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
			t.Error("lease expiry must not reconnect a healthy session")
			return nil, ErrReplacement
		})
		if err != nil {
			t.Fatal(err)
		}
		defer continuation.Close()
		directory, release, err := continuation.AcquireDirectory(context.Background(), runtime.Descriptor().SyntheticRoot())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		file, _ := directory.Pages()[0].Entries()[0].FileID()
		opened, err := continuation.OpenRevision(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		var downloaded []byte
		sink := transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
			if offset != uint64(len(downloaded)) {
				t.Fatalf("download restarted at %d after %d bytes", offset, len(downloaded))
			}
			downloaded = append(downloaded, data...)
			return nil
		})
		// Advance the real renewal timers in virtual time. Each read takes a new block
		// so the broker cache cannot conceal a dead wire lease.
		chunk := uint64(opened.Descriptor.Geometry().ChunkSize())
		for offset := uint64(0); offset < opened.Descriptor.ExactSize(); {
			end := min(offset+chunk, opened.Descriptor.ExactSize())
			if err := continuation.ReadRange(context.Background(), opened.Handle, opened.Descriptor, content.Range{Offset: offset, End: end}, sink); err != nil {
				t.Fatal(err)
			}
			offset = end
			if offset < opened.Descriptor.ExactSize() {
				// The first failed renewal occurs within the final TTL before the cap.
				time.Sleep(content.MaxLeaseLifetime - content.LeaseTTL + content.LeaseRenewWindow)
				synctest.Wait()
			}
		}
		if !bytes.Equal(downloaded, f.payload) {
			t.Fatalf("downloaded %d incorrect bytes", len(downloaded))
		}
		if runtime.ProtocolSessionID() != sessionID || continuation.Runtime() != runtime {
			t.Fatal("lease replacement changed the session")
		}
		if err := continuation.ReleaseRevision(context.Background(), opened.Handle); err != nil {
			t.Fatal(err)
		}
	})
}
