package catalog

// MaxSourceOmissionSamples bounds local feedback without traversing omitted subtrees.
const MaxSourceOmissionSamples = 16

type SourceOmissionReason string

const (
	SourceOmissionNone         SourceOmissionReason = ""
	SourceOmissionSymbolicLink SourceOmissionReason = "symbolic_link"
	SourceOmissionReparsePoint SourceOmissionReason = "reparse_point"
	SourceOmissionSpecialFile  SourceOmissionReason = "special_file"
	SourceOmissionName         SourceOmissionReason = "unrepresentable_name"
)

func (reason SourceOmissionReason) Valid() bool {
	switch reason {
	case SourceOmissionSymbolicLink, SourceOmissionReparsePoint, SourceOmissionSpecialFile, SourceOmissionName:
		return true
	default:
		return false
	}
}

type SourceOmission struct {
	Name   string
	Reason SourceOmissionReason
}

// SourceOmissions is local source feedback after a stable directory scan.
// Native names and paths are deliberately not catalog names or wire metadata:
// the rejected names cannot be represented by the catalog grammar.
type SourceOmissions struct {
	Directory     DirectoryID
	Generation    DirectoryGeneration
	Attempt       ScanAttemptID
	DirectoryPath string
	Count         uint64
	Samples       []SourceOmission
}

// Observers must return promptly; presentation must not backpressure discovery.
type SourceOmissionObserver interface {
	ObserveSourceOmissions(SourceOmissions)
}

type SourceOmissionObserverFunc func(SourceOmissions)

func (observe SourceOmissionObserverFunc) ObserveSourceOmissions(value SourceOmissions) {
	if observe != nil {
		observe(value)
	}
}
