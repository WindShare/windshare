package desktop

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
)

func TestSenderTraceFollowsAdmissionAndStopsWithoutDiscardingRecords(t *testing.T) {
	a, _, _ := fixture(t)
	stream, err := diagnostics.NewStream(4, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.deps.senderTrace = &senderTrace{stream: stream}
	var admitted diagnostics.Sink
	a.deps.start = func(_ context.Context, request engine.ShareRequest) (shareTask, error) {
		admitted = request.Diagnostics
		return nil, errors.New("fixture stops at admission")
	}
	start := func(id string) diagnostics.Sink {
		t.Helper()
		request := createRequest()
		request.OperationID = id
		if reply := a.execute(request); reply.Error != nil {
			t.Fatal(reply.Error)
		}
		a.work.Wait()
		return admitted
	}
	set := func(enabled bool, want SenderTraceState) uint64 {
		t.Helper()
		reply := a.execute(Request{Command: "set_sender_trace", SenderTrace: &enabled})
		if reply.Error != nil || reply.Snapshot.SenderTrace != want {
			t.Fatalf("trace status should be %q: %+v", want, reply)
		}
		return reply.Snapshot.Revision
	}
	initial := start("ordinary")
	firstRevision := set(true, SenderTraceOn)
	if initial.Enabled() || set(true, SenderTraceOn) != firstRevision {
		t.Fatal("enabling trace changed an old admission or replayed state")
	}
	first := start("first-capture")
	sample := diagnostics.New("sample", nil)
	if !first.Emit(sample) {
		t.Fatal("new share was missing diagnostics")
	}
	set(false, SenderTraceOff)
	set(true, SenderTraceOn)
	if first.Enabled() || !start("second-capture").Enabled() {
		t.Fatal("a new capture must leave prior sinks stopped")
	}
	if cut := stream.Close(); cut.Enqueued != 1 {
		t.Fatalf("stopping trace lost queued evidence: %+v", cut)
	}
}
