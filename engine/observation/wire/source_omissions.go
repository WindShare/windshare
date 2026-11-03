package wire

import appobs "github.com/windshare/windshare/engine/observation"

type SourceOmissionSample struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

type SourceOmissionsPayload struct {
	DirectoryID      string                 `json:"directory_id"`
	GenerationID     string                 `json:"generation_id"`
	ScanAttemptID    string                 `json:"scan_attempt_id"`
	DirectoryPath    string                 `json:"directory_path"`
	OmittedEntries   string                 `json:"omitted_entries"`
	UnsampledEntries string                 `json:"unsampled_entries"`
	Samples          []SourceOmissionSample `json:"samples"`
}

func (SourceOmissionsPayload) ObservationPayload() {}

func (enc *encoder) encodeSourceOmissions(event appobs.SourceOmissionsObserved) error {
	summary := event.Summary()
	samples := make([]SourceOmissionSample, len(summary.Samples))
	for index, sample := range summary.Samples {
		samples[index] = SourceOmissionSample{Name: sample.Name, Reason: string(sample.Reason), Detail: sample.Detail}
	}
	enc.set("source_omissions", nil, SourceOmissionsPayload{
		DirectoryID:   EncodeIdentity(summary.Directory.Bytes()),
		GenerationID:  EncodeIdentity(summary.Generation.Bytes()),
		ScanAttemptID: EncodeIdentity(summary.Attempt.Bytes()),
		DirectoryPath: summary.DirectoryPath, OmittedEntries: decimal(summary.Count),
		UnsampledEntries: decimal(summary.Count - uint64(len(samples))), Samples: samples,
	})
	return nil
}
