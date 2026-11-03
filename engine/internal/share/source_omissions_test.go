package share

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine/internal/task"
)

func TestSourceOmissionsReachProductObservationsWithoutDiagnostics(t *testing.T) {
	var events []task.Event
	observations := newObservations(task.Control{Emit: func(event task.Event) bool {
		events = append(events, event)
		return true
	}}, Request{})
	defer observations.complete()
	factory := observations.sourceFactory(liveshare.FileSourceFactoryFunc(func(_ context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		if source.Diagnostics.Enabled() || source.Omissions == nil {
			t.Fatal("source feedback depends on enabling diagnostic recording")
		}
		source.Omissions.ObserveSourceOmissions(catalog.SourceOmissions{Count: 2})
		return nil, nil
	}))
	if _, err := factory.OpenFileSource(t.Context(), liveshare.FileSourceContext{}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("product events=%+v", events)
	}
	event, ok := events[0].(SourceOmissionsObserved)
	if !ok || event.Summary.Count != 2 {
		t.Fatalf("omission fact lost: %+v", events[0])
	}
}
