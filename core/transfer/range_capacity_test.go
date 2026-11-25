package transfer

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

type changingJobSession struct {
	mu sync.Mutex
	id protocolsession.ProtocolSessionID
}

func (s *changingJobSession) ProtocolSessionID() protocolsession.ProtocolSessionID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *changingJobSession) set(id protocolsession.ProtocolSessionID) {
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
}

type capacityRangeFunc func(context.Context, RevisionHandle, content.FileRevisionDescriptor, content.Range, RangeSink) error

func (f capacityRangeFunc) ReadRange(ctx context.Context, l RevisionHandle, d content.FileRevisionDescriptor, r content.Range, s RangeSink) error {
	return f(ctx, l, d, r, s)
}

func TestReplacementLeaseCapacityUsesExistingJobBudgetAndRebuildsOnlyUncommittedRange(t *testing.T) {
	file := transferID[catalog.FileID](84)
	wait, timers := capacityJobWaitConfig(t, 10*time.Second)
	job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, &capacityRevisionScript{}, wait, diagnostics.Sink{})
	session := &changingJobSession{id: transferID[protocolsession.ProtocolSessionID](89)}
	job.session = session
	opened := OpenedRevision{Handle: transferID[RevisionHandle](86), Descriptor: jobDescriptor(t, transferID[catalog.ShareInstance](80), file, 88, 6)}
	calls := 0
	job.blocks = capacityRangeFunc(func(ctx context.Context, _ RevisionHandle, _ content.FileRevisionDescriptor, _ content.Range, sink RangeSink) error {
		calls++
		if calls == 1 {
			session.set(transferID[protocolsession.ProtocolSessionID](91))
			if err := sink.WriteRange(ctx, 0, []byte("abc")); err != nil {
				t.Fatal(err)
			}
			return capacityJobSignal(t, 2*time.Second)
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
	if job.protocolSessionID() != session.ProtocolSessionID() {
		t.Fatal("trace retained retired protocol session")
	}
}

func TestReadRequestedRangeHandlesSequentialAndSupersededSessionReplacement(t *testing.T) {
	file := transferID[catalog.FileID](84)
	first := transferID[protocolsession.ProtocolSessionID](89)
	second := transferID[protocolsession.ProtocolSessionID](91)
	foreign := transferID[protocolsession.ProtocolSessionID](92)
	opened := OpenedRevision{
		Handle:     transferID[RevisionHandle](86),
		Descriptor: jobDescriptor(t, transferID[catalog.ShareInstance](80), file, 88, 6),
	}

	t.Run("sequential replacement and superseded signal succeed on fresh session", func(t *testing.T) {
		session := &changingJobSession{id: first}
		wait, timers := dynamicSessionWaitConfig(t, session, 10*time.Second)
		job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, &capacityRevisionScript{}, wait, diagnostics.Sink{})
		job.session = session
		calls := 0
		job.blocks = capacityRangeFunc(func(ctx context.Context, _ RevisionHandle, _ content.FileRevisionDescriptor, _ content.Range, sink RangeSink) error {
			calls++
			switch calls {
			case 1:
				return capacitySignalForSession(t, first, 2*time.Second)
			case 2:
				session.set(second)
				return capacitySignalForSession(t, first, 2*time.Second)
			case 3:
				return capacitySignalForSession(t, second, time.Second)
			default:
				return sink.WriteRange(ctx, 0, []byte("abcdef"))
			}
		})
		var written []byte
		run := &jobRun{job: job}
		buffered, err := run.readRequestedRange(context.Background(), plannedFile{file: file}, opened, content.Range{End: 6}, RangeSinkFunc(func(_ context.Context, _ uint64, value []byte) error {
			written = append(written, value...)
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := buffered.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if calls != 4 || !bytes.Equal(written, []byte("abcdef")) || !slices.Equal(timers.waits, []time.Duration{2 * time.Second, time.Second}) {
			t.Fatalf("calls=%d written=%q waits=%v", calls, written, timers.waits)
		}
		if snap := job.revisionWait.Snapshot(); snap.ActiveWaiters != 0 || snap.AccumulatedWait != 3*time.Second || snap.Attempts != 2 {
			t.Fatalf("waiter snapshot=%+v", snap)
		}
	})

	t.Run("foreign signal in range read fails closed", func(t *testing.T) {
		session := &changingJobSession{id: first}
		wait, timers := dynamicSessionWaitConfig(t, session, 10*time.Second)
		job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, &capacityRevisionScript{}, wait, diagnostics.Sink{})
		job.session = session
		calls := 0
		job.blocks = capacityRangeFunc(func(context.Context, RevisionHandle, content.FileRevisionDescriptor, content.Range, RangeSink) error {
			calls++
			return capacitySignalForSession(t, foreign, 2*time.Second)
		})
		run := &jobRun{job: job}
		_, err := run.readRequestedRange(context.Background(), plannedFile{file: file}, opened, content.Range{End: 6}, RangeSinkFunc(func(context.Context, uint64, []byte) error {
			return nil
		}))
		if !isSessionCode(fault.FaultOf(err), fault.SessionDependencyContract) || !errors.Is(err, ErrRevisionIdentity) || calls != 1 || len(timers.waits) != 0 {
			t.Fatalf("err=%v calls=%d waits=%v", err, calls, timers.waits)
		}
	})
}
