package content

import (
	"crypto/sha256"
	"errors"
)

const sourceMetadataDomain = "windshare/source-metadata/v1"

// SourceMetadata is an opaque, share/file-bound comparison token, not a content
// digest. A zero token means the provider supplied insufficient reopen evidence.
type SourceMetadata [sha256.Size]byte

func (m SourceMetadata) IsZero() bool  { return m == SourceMetadata{} }
func (m SourceMetadata) Bytes() []byte { return append([]byte(nil), m[:]...) }

func SourceMetadataFromBytes(raw []byte) (SourceMetadata, error) {
	var metadata SourceMetadata
	if len(raw) != len(metadata) {
		return metadata, errors.New("source metadata must be 32 bytes")
	}
	copy(metadata[:], raw)
	return metadata, nil
}

func sourceMetadata(evidence RevisionEvidence) SourceMetadata {
	if !evidence.ModifiedTime().Present() {
		return SourceMetadata{}
	}
	// Open-instance nonces belong exclusively to encryption identity. The
	// verified provider candidate remains useful for best-effort plaintext reuse.
	hash := sha256.New()
	_, _ = hash.Write([]byte(sourceMetadataDomain))
	_, _ = hash.Write(canonicalRevisionEvidence(evidence))
	var metadata SourceMetadata
	copy(metadata[:], hash.Sum(nil))
	return metadata
}

// BindRevisionMetadata carries the source's comparison evidence inside the
// authenticated descriptor. It grants neither byte equality nor output authority.
func BindRevisionMetadata(descriptor FileRevisionDescriptor, metadata SourceMetadata) FileRevisionDescriptor {
	descriptor.metadata = metadata
	return descriptor
}

type RevisionComparison uint8

const (
	RevisionComparisonUnknown RevisionComparison = iota
	RevisionComparisonMetadataAgreement
	RevisionComparisonByteAgreement
	RevisionComparisonMismatch
	RevisionComparisonUnavailable
)

type RevisionComparisonReason string

const (
	ComparisonSameRevision        RevisionComparisonReason = "same-revision"
	ComparisonMetadataAgreement   RevisionComparisonReason = "metadata-agreement"
	ComparisonContextChanged      RevisionComparisonReason = "context-changed"
	ComparisonSizeChanged         RevisionComparisonReason = "size-changed"
	ComparisonMetadataChanged     RevisionComparisonReason = "metadata-changed"
	ComparisonMetadataUnavailable RevisionComparisonReason = "metadata-unavailable"
)

// RevisionComparisonEvidence describes applicability to the current revision;
// authentication and durable/local ownership checks remain separate gates.
type RevisionComparisonEvidence struct {
	Comparison RevisionComparison
	Reason     RevisionComparisonReason
}

func (e RevisionComparisonEvidence) AllowsReuse() bool {
	return e.Comparison == RevisionComparisonMetadataAgreement || e.Comparison == RevisionComparisonByteAgreement
}

// RevisionSourceEvidence is the part of an authenticated revision retained by output checkpoints.
type RevisionSourceEvidence struct {
	FileRevision   FileRevision
	ExactSize      uint64
	SourceMetadata SourceMetadata
}

func (descriptor FileRevisionDescriptor) SourceEvidence() RevisionSourceEvidence {
	return RevisionSourceEvidence{descriptor.FileRevision(), descriptor.ExactSize(), descriptor.SourceMetadata()}
}

func CompareRevisionMetadata(previous, current RevisionSourceEvidence) RevisionComparisonEvidence {
	if previous.FileRevision.IsZero() || current.FileRevision.IsZero() {
		return RevisionComparisonEvidence{RevisionComparisonUnavailable, ComparisonMetadataUnavailable}
	}
	if previous.ExactSize != current.ExactSize {
		return RevisionComparisonEvidence{RevisionComparisonMismatch, ComparisonSizeChanged}
	}
	if !previous.SourceMetadata.IsZero() && !current.SourceMetadata.IsZero() && previous.SourceMetadata != current.SourceMetadata {
		return RevisionComparisonEvidence{RevisionComparisonMismatch, ComparisonMetadataChanged}
	}
	if previous.FileRevision == current.FileRevision {
		return RevisionComparisonEvidence{RevisionComparisonMetadataAgreement, ComparisonSameRevision}
	}
	if previous.SourceMetadata.IsZero() || current.SourceMetadata.IsZero() {
		return RevisionComparisonEvidence{RevisionComparisonUnavailable, ComparisonMetadataUnavailable}
	}
	return RevisionComparisonEvidence{RevisionComparisonMetadataAgreement, ComparisonMetadataAgreement}
}

func CompareRevisions(previous, current FileRevisionDescriptor) RevisionComparisonEvidence {
	if previous.ShareInstance() != current.ShareInstance() || previous.FileID() != current.FileID() ||
		previous.Geometry().ChunkSize() != current.Geometry().ChunkSize() {
		return RevisionComparisonEvidence{RevisionComparisonMismatch, ComparisonContextChanged}
	}
	if previous.ModifiedTime().Present() && current.ModifiedTime().Present() && previous.ModifiedTime() != current.ModifiedTime() {
		return RevisionComparisonEvidence{RevisionComparisonMismatch, ComparisonMetadataChanged}
	}
	return CompareRevisionMetadata(previous.SourceEvidence(), current.SourceEvidence())
}

type revisionComparisonError struct {
	comparison RevisionComparison
	err        error
}

func (e revisionComparisonError) Error() string                          { return e.err.Error() }
func (e revisionComparisonError) Unwrap() error                          { return e.err }
func (e revisionComparisonError) RevisionComparison() RevisionComparison { return e.comparison }

// WithRevisionComparison preserves the provider's observation independently of
// retry and protocol errors; unavailable evidence must not become observed drift.
func WithRevisionComparison(err error, comparison RevisionComparison) error {
	if err == nil {
		return nil
	}
	if comparison != RevisionComparisonMismatch && comparison != RevisionComparisonUnavailable {
		return err
	}
	return revisionComparisonError{comparison: comparison, err: err}
}

func RevisionComparisonOf(err error) RevisionComparison {
	if err == nil {
		return RevisionComparisonMetadataAgreement
	}
	type classified interface {
		RevisionComparison() RevisionComparison
	}
	var marker classified
	if errors.As(err, &marker) {
		comparison := marker.RevisionComparison()
		if comparison == RevisionComparisonMismatch || comparison == RevisionComparisonUnavailable {
			return comparison
		}
	}
	if errors.Is(err, ErrSourceDrift) || errors.Is(err, ErrRevisionStale) {
		return RevisionComparisonMismatch
	}
	return RevisionComparisonUnavailable
}
