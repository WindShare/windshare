package eventproject

import (
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
)

func TestSourceOmissionsPreserveBoundedSamplesAndScanIdentityAcrossProjection(t *testing.T) {
	summary := catalog.SourceOmissions{
		Directory: catalog.DirectoryID{1}, Generation: catalog.DirectoryGeneration{2}, Attempt: catalog.ScanAttemptID{3},
		DirectoryPath: "/source/nested", Count: 3,
		Samples: []catalog.SourceOmission{{Name: "family\u200dphotos", Reason: catalog.SourceOmissionName}},
	}
	var published task.Observation
	accepted := Workflow(appobs.WorkflowShare, task.Observation{TaskID: "share:1", Event: share.SourceOmissionsObserved{Summary: summary}}, func(value task.Observation) bool {
		published = value
		return true
	})
	event, ok := published.Event.(appobs.SourceOmissionsObserved)
	if !accepted || !ok || published.TaskID != "share:1" || event.Level() != appobs.LevelWarning {
		t.Fatalf("omission observation lost: %+v", published)
	}
	summary.Samples[0].Name = "mutated"
	copy := event.Summary()
	copy.Samples[0].Name = "also mutated"
	encoded, err := wire.Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := encoded.Payload.(wire.SourceOmissionsPayload)
	if !ok || encoded.Event != "source_omissions" || encoded.Workflow != "share" ||
		payload.DirectoryID != wire.EncodeIdentity(summary.Directory.Bytes()) ||
		payload.GenerationID != wire.EncodeIdentity(summary.Generation.Bytes()) ||
		payload.ScanAttemptID != wire.EncodeIdentity(summary.Attempt.Bytes()) ||
		payload.DirectoryPath != summary.DirectoryPath || payload.OmittedEntries != "3" || payload.UnsampledEntries != "2" ||
		len(payload.Samples) != 1 || payload.Samples[0].Name != "family\u200dphotos" || payload.Samples[0].Reason != "unrepresentable_name" {
		t.Fatalf("omission serialization lost source facts or sample ownership: %+v", encoded)
	}

	for _, mutate := range []func(*catalog.SourceOmissions){
		func(value *catalog.SourceOmissions) { value.Count = 0 },
		func(value *catalog.SourceOmissions) { value.Attempt = catalog.ScanAttemptID{} },
		func(value *catalog.SourceOmissions) { value.DirectoryPath = "" },
		func(value *catalog.SourceOmissions) { value.Samples = nil },
		func(value *catalog.SourceOmissions) {
			value.Samples[0].Reason = catalog.SourceOmissionReason("unknown")
		},
		func(value *catalog.SourceOmissions) { value.Samples[0].Name = "" },
		func(value *catalog.SourceOmissions) {
			value.Samples = make([]catalog.SourceOmission, catalog.MaxSourceOmissionSamples+1)
		},
	} {
		invalid := event.Summary()
		mutate(&invalid)
		if _, err := appobs.NewSourceOmissionsObserved(invalid); err == nil {
			t.Fatalf("invalid source report accepted: %+v", invalid)
		}
	}
}
