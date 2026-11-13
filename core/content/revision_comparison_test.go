package content

import (
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSourceMetadataSeparatesReopenFromObservedChange(t *testing.T) {
	evidence := knownRevisionEvidence(t)
	baseline := sourceMetadata(evidence)
	reopened := evidence
	reopened.openInstance[0] = 1
	if baseline.IsZero() || sourceMetadata(reopened) != baseline {
		t.Fatal("open-instance identity contaminated source metadata")
	}
	mutations := map[string]func(*RevisionEvidence){
		"share":   func(e *RevisionEvidence) { e.shareInstance[0]++ },
		"file":    func(e *RevisionEvidence) { e.fileID[0]++ },
		"size":    func(e *RevisionEvidence) { e.expectedSize++ },
		"chunk":   func(e *RevisionEvidence) { e.chunkSize *= 2 },
		"source":  func(e *RevisionEvidence) { e.sourceIdentity, _ = catalog.NewSourceIdentity([]byte("replacement")) },
		"version": func(e *RevisionEvidence) { e.versionCandidate, _ = catalog.NewVersionCandidate([]byte("changed")) },
		"modified": func(e *RevisionEvidence) {
			e.modifiedTime, _ = catalog.NewModifiedTime(1, 0, catalog.TimePrecisionSeconds)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := evidence
			mutate(&changed)
			if sourceMetadata(changed) == baseline {
				t.Fatal("comparison lost source context")
			}
		})
	}
	evidence.modifiedTime = catalog.ModifiedTime{}
	if !sourceMetadata(evidence).IsZero() {
		t.Fatal("missing modification evidence authorized metadata reuse")
	}
}

func TestRevisionComparisonExplainsApplicabilityWithoutClaimingByteAgreement(t *testing.T) {
	evidence := knownRevisionEvidence(t)
	geometry, _ := NewFileGeometry(evidence.ExpectedSize(), evidence.ChunkSize())
	previous, _ := NewFileRevisionDescriptor(evidence.ShareInstance(), evidence.FileID(), catalogID[FileRevision](20), geometry, evidence.ModifiedTime())
	previous = BindRevisionMetadata(previous, sourceMetadata(evidence))
	current := previous
	current.revision[0]++
	tests := []struct {
		name     string
		current  FileRevisionDescriptor
		expected RevisionComparisonEvidence
	}{
		{"same revision", previous, RevisionComparisonEvidence{RevisionComparisonMetadataAgreement, ComparisonSameRevision}},
		{"reopened", current, RevisionComparisonEvidence{RevisionComparisonMetadataAgreement, ComparisonMetadataAgreement}},
	}
	missing := current
	missing.metadata = SourceMetadata{}
	changed := current
	changed.metadata[0]++
	context := current
	context.fileID[0]++
	tests = append(tests,
		struct {
			name     string
			current  FileRevisionDescriptor
			expected RevisionComparisonEvidence
		}{"unavailable", missing, RevisionComparisonEvidence{RevisionComparisonUnavailable, ComparisonMetadataUnavailable}},
		struct {
			name     string
			current  FileRevisionDescriptor
			expected RevisionComparisonEvidence
		}{"modified", changed, RevisionComparisonEvidence{RevisionComparisonMismatch, ComparisonMetadataChanged}},
		struct {
			name     string
			current  FileRevisionDescriptor
			expected RevisionComparisonEvidence
		}{"file", context, RevisionComparisonEvidence{RevisionComparisonMismatch, ComparisonContextChanged}},
	)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CompareRevisions(previous, test.current)
			if got != test.expected {
				t.Fatalf("comparison=%+v want=%+v", got, test.expected)
			}
		})
	}
}
