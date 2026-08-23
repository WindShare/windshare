package capacitytrace

import (
	"testing"

	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type recordingTracer struct {
	events int
}

func (tracer *recordingTracer) TraceCapacity(revisioncapacity.TraceEvent) {
	tracer.events++
}

func TestRouterStaleReleasePreservesSuccessorBinding(t *testing.T) {
	router := &Router{}
	first := &recordingTracer{}
	second := &recordingTracer{}
	releaseFirst := router.Bind(first)
	router.TraceCapacity(revisioncapacity.TraceEvent{})

	releaseSecond := router.Bind(second)
	releaseFirst()
	router.TraceCapacity(revisioncapacity.TraceEvent{})

	if first.events != 1 || second.events != 1 {
		t.Fatalf("routed events = first %d, second %d", first.events, second.events)
	}
	releaseSecond()
	releaseSecond()
	router.TraceCapacity(revisioncapacity.TraceEvent{})
	if second.events != 1 {
		t.Fatalf("released sink received %d events", second.events)
	}
}

func TestRouterNilBindingDoesNotDisplaceActiveSink(t *testing.T) {
	router := &Router{}
	sink := &recordingTracer{}
	release := router.Bind(sink)
	noopRelease := router.Bind(nil)
	noopRelease()
	router.TraceCapacity(revisioncapacity.TraceEvent{})

	if sink.events != 1 {
		t.Fatalf("active sink received %d events", sink.events)
	}
	release()

	var nilRouter *Router
	nilRouter.TraceCapacity(revisioncapacity.TraceEvent{})
	nilRouter.Bind(sink)()
}
