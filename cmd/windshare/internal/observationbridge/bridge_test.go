package observationbridge

import (
	"context"
	"reflect"
	"testing"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

type recordedLoss struct {
	category clievent.ObserverLossCategory
	reason   clievent.ObserverLossReason
	count    uint64
}

type recordingLossSink struct {
	reported []recordedLoss
}

func (sink *recordingLossSink) ReportObserverLoss(
	category clievent.ObserverLossCategory,
	reason clievent.ObserverLossReason,
	count uint64,
) bool {
	sink.reported = append(sink.reported, recordedLoss{category: category, reason: reason, count: count})
	return true
}

func TestCumulativeLossesReportIndependentSnapshotDeltas(t *testing.T) {
	var sink recordingLossSink
	losses := NewCumulativeLosses[string](&sink)
	losses.Report("relay", clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 2)
	losses.Report("relay", clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 2)
	losses.Report("relay", clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 5)
	losses.Report("webrtc", clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 1)
	losses.Report("ignored", clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 0)

	want := []recordedLoss{
		{clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 2},
		{clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 3},
		{clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 1},
	}
	if !reflect.DeepEqual(sink.reported, want) {
		t.Fatalf("reported losses = %#v, want %#v", sink.reported, want)
	}
	var nilLosses *CumulativeLosses[string]
	nilLosses.Report("ignored", clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, 1)
	if NewCumulativeLosses[string](nil) != nil {
		t.Fatal("nil sink created cumulative loss accounting")
	}
}

func TestReaderDrainsCompletedStreamInFIFOOrder(t *testing.T) {
	stream := make(chan int, 3)
	stream <- 1
	stream <- 2
	stream <- 3
	close(stream)
	var forwarded []int
	reader := Start(stream, &PublicationGate{}, func(_ context.Context, value int) {
		forwarded = append(forwarded, value)
	})
	status := reader.Join(context.Background())
	if !status.Joined || status.Forwarded != 3 || status.Buffered != 0 || status.Active {
		t.Fatalf("status = %+v", status)
	}
	if !reflect.DeepEqual(forwarded, []int{1, 2, 3}) {
		t.Fatalf("forwarded = %v", forwarded)
	}
	if repeated := reader.Join(context.Background()); repeated != status {
		t.Fatalf("repeated status = %+v, want %+v", repeated, status)
	}
}

func TestReaderDeadlineReportsBoundedResidueWithoutClaimingProjectorTermination(t *testing.T) {
	stream := make(chan int, 2)
	stream <- 1
	stream <- 2
	close(stream)
	entered := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	gate := &PublicationGate{}
	reader := Start(stream, gate, func(context.Context, int) {
		close(entered)
		<-release
		close(returned)
	})
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status := reader.Join(ctx)
	if status.Joined || !status.Active || status.Buffered != 1 || status.Forwarded != 0 {
		t.Fatalf("status = %+v", status)
	}
	if gate.Commit(context.Background(), func() bool { return true }) {
		t.Fatal("publication gate remained open after the join cut")
	}
	select {
	case <-returned:
		t.Fatal("join claimed control over the blocked projector")
	default:
	}
	close(release)
	<-returned
}

func TestReaderDeadlineCancelsIdleReaderAndNilReaderIsAlreadyJoined(t *testing.T) {
	stream := make(chan int, 1)
	reader := Start(stream, nil, func(context.Context, int) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if status := reader.Join(ctx); status.Joined || status.Active || status.Buffered != 0 {
		t.Fatalf("idle deadline status = %+v", status)
	}
	var nilReader *Reader[int]
	if status := nilReader.Join(context.Background()); !status.Joined {
		t.Fatalf("nil reader status = %+v", status)
	}
}

func TestStartRejectsMissingStreamOrProjection(t *testing.T) {
	stream := make(chan int)
	if Start(nil, nil, func(context.Context, int) {}) != nil {
		t.Fatal("nil stream created a reader")
	}
	if Start(stream, nil, nil) != nil {
		t.Fatal("nil projection created a reader")
	}
}
