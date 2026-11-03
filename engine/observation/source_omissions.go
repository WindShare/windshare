package observation

import (
	"slices"

	"github.com/windshare/windshare/core/catalog"
)

type SourceOmissionsObserved struct{ summary catalog.SourceOmissions }

func NewSourceOmissionsObserved(summary catalog.SourceOmissions) (SourceOmissionsObserved, error) {
	value := SourceOmissionsObserved{summary: summary}
	if err := value.Validate(); err != nil {
		return SourceOmissionsObserved{}, err
	}
	value.summary.Samples = slices.Clone(summary.Samples)
	return value, nil
}

func (SourceOmissionsObserved) EngineEvent()       {}
func (SourceOmissionsObserved) Workflow() Workflow { return WorkflowShare }
func (SourceOmissionsObserved) Level() Level       { return LevelWarning }

func (value SourceOmissionsObserved) Summary() catalog.SourceOmissions {
	summary := value.summary
	summary.Samples = slices.Clone(summary.Samples)
	return summary
}

func (value SourceOmissionsObserved) Validate() error {
	summary := value.summary
	if summary.Directory.IsZero() || summary.Generation.IsZero() || summary.Attempt.IsZero() ||
		summary.DirectoryPath == "" || summary.Count == 0 ||
		len(summary.Samples) == 0 || len(summary.Samples) > catalog.MaxSourceOmissionSamples ||
		uint64(len(summary.Samples)) > summary.Count {
		return ErrInvalidEvent
	}
	for _, sample := range summary.Samples {
		if sample.Name == "" || !sample.Reason.Valid() {
			return ErrInvalidEvent
		}
	}
	return nil
}
