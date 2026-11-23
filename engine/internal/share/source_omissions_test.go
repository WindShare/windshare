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

func TestControllerAggregatesSourceOmissionsByDirectoryIDAndNotifiesOnChange(t *testing.T) {
	controller := NewController()
	notifications := 0
	obs := newObservations(task.Control{
		StateChanged: func() { notifications++ },
	}, Request{})
	obs.bindController(controller)
	defer obs.complete()

	dirA, _ := catalog.DirectoryIDFromBytes([]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	dirB, _ := catalog.DirectoryIDFromBytes([]byte{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2})

	// Zero-count report must not notify or populate snapshot summary.
	obs.ObserveSourceOmissions(catalog.SourceOmissions{Directory: dirA, Count: 0})
	if notifications != 0 {
		t.Fatalf("zero omissions triggered %d notifications", notifications)
	}
	if _, ok := controller.SourceOmissions(); ok {
		t.Fatal("zero omissions produced a summary")
	}

	obs.ObserveSourceOmissions(catalog.SourceOmissions{
		Directory: dirA,
		Count:     2,
		Samples: []catalog.SourceOmission{
			{Name: "a.docx", Reason: catalog.SourceOmissionCloudPlaceholder},
			{Name: "link", Reason: catalog.SourceOmissionSymbolicLink},
		},
	})
	// Duplicate report for the same DirectoryID is idempotent and does not notify again.
	obs.ObserveSourceOmissions(catalog.SourceOmissions{
		Directory: dirA,
		Count:     2,
		Samples:   []catalog.SourceOmission{{Name: "a.docx", Reason: catalog.SourceOmissionCloudPlaceholder}},
	})
	if notifications != 1 {
		t.Fatalf("notifications = %d, want 1", notifications)
	}

	// Second distinct directory aggregates count and bounds retained samples.
	manySamples := make([]catalog.SourceOmission, catalog.MaxSourceOmissionSamples)
	for i := range manySamples {
		manySamples[i] = catalog.SourceOmission{Name: "cloud.bin", Reason: catalog.SourceOmissionCloudPlaceholder}
	}
	obs.ObserveSourceOmissions(catalog.SourceOmissions{
		Directory: dirB,
		Count:     uint64(len(manySamples)),
		Samples:   manySamples,
	})
	if notifications != 2 {
		t.Fatalf("notifications = %d, want 2", notifications)
	}
	summary, ok := controller.SourceOmissions()
	if !ok || summary.Count != uint64(2+len(manySamples)) || len(summary.Samples) != catalog.MaxSourceOmissionSamples {
		t.Fatalf("summary = %+v (ok=%v)", summary, ok)
	}
	if summary.Samples[0].Name != "a.docx" || summary.Samples[0].Reason != catalog.SourceOmissionCloudPlaceholder {
		t.Fatalf("first sample = %+v", summary.Samples[0])
	}
}
