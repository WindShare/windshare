package transfer

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"time"
)

type changingJobSession struct {
	id protocolsession.ProtocolSessionID
}

func (s *changingJobSession) ProtocolSessionID() protocolsession.ProtocolSessionID { return s.id }

type capacityRangeFunc func(context.Context, content.LeaseID, content.FileRevisionDescriptor, content.Range, RangeSink) error

func (f capacityRangeFunc) ReadRange(ctx context.Context, l content.LeaseID, d content.FileRevisionDescriptor, r content.Range, s RangeSink) error {
	return f(ctx, l, d, r, s)
}

func TestReplacementLeaseCapacityUsesExistingJobBudgetAndRebuildsOnlyUncommittedRange(t *testing.T) {
	file := transferID[catalog.FileID](84)
	wait, token, timers := capacityJobWaitConfig(t, 10*time.Second)
	job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, &capacityRevisionScript{}, wait, nil)
	session := &changingJobSession{id: transferID[protocolsession.ProtocolSessionID](89)}
	job.session = session
	opened := OpenedRevision{LeaseID: transferID[content.LeaseID](86), Descriptor: jobDescriptor(t, transferID[catalog.ShareInstance](80), file, 88, 6)}
	calls := 0
	job.blocks = capacityRangeFunc(func(ctx context.Context, _ content.LeaseID, _ content.FileRevisionDescriptor, _ content.Range, sink RangeSink) error {
		calls++
		if calls == 1 {
			session.id = transferID[protocolsession.ProtocolSessionID](91)
			if err := sink.WriteRange(ctx, 0, []byte("abc")); err != nil {
				t.Fatal(err)
			}
			return capacityJobSignal(t, token, 2*time.Second)
		}
		return sink.WriteRange(ctx, 0, []byte("abcdef"))
	})
	var written []byte
	run := &jobRun{job: job}
	buffered, err := run.readRequestedRange(context.Background(), plannedFile{file: file}, opened, content.Range{End: 6}, RangeSinkFunc(func(_ context.Context, _ uint64, value []byte) error { written = append(written, value...); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Fatal("uncommitted retry bytes reached output")
	}
	if err = buffered.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !bytes.Equal(written, []byte("abcdef")) {
		t.Fatalf("calls=%d output=%q", calls, written)
	}
	if len(timers.waits) != 1 || job.revisionWait.Snapshot().AccumulatedWait != 2*time.Second {
		t.Fatal("capacity wait owner was replaced")
	}
	if job.protocolSessionID() != session.id {
		t.Fatal("trace retained retired protocol session")
	}
}
