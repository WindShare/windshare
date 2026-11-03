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
	SourceOmissionAccessDenied SourceOmissionReason = "access_denied"
)

func (reason SourceOmissionReason) Valid() bool {
	switch reason {
	case SourceOmissionSymbolicLink, SourceOmissionReparsePoint, SourceOmissionSpecialFile, SourceOmissionName, SourceOmissionAccessDenied:
		return true
	default:
		return false
	}
}

type SourceOmission struct {
	Name   string
	Reason SourceOmissionReason
	// Detail explains the rejected spelling, including invisible scalars and byte limits.
	Detail string
}

// SourceOmissions is local source feedback after a stable directory scan.
// Native names and paths stay out of catalog wire metadata because some omitted
// names cannot be represented by the catalog grammar. Count measures immediate
// children; an omitted directory excludes its subtree without traversing it.
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
