package catalog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/internal/unicode15"
)

const (
	// MaxSourceOmissionSamples bounds local feedback without traversing omitted subtrees.
	MaxSourceOmissionSamples  = 16
	maxCollisionScalarPreview = 4
)

type SourceOmissionReason string

const (
	SourceOmissionNone         SourceOmissionReason = ""
	SourceOmissionSymbolicLink SourceOmissionReason = "symbolic_link"
	SourceOmissionReparsePoint SourceOmissionReason = "reparse_point"
	SourceOmissionSpecialFile  SourceOmissionReason = "special_file"
	SourceOmissionName         SourceOmissionReason = "unrepresentable_name"
	// SourceOmissionNameCollision marks every member of a sibling group whose names
	// match under portable name matching; none can be preferred without guessing.
	SourceOmissionNameCollision SourceOmissionReason = "name_collision"
	SourceOmissionAccessDenied  SourceOmissionReason = "access_denied"
)

func (reason SourceOmissionReason) Valid() bool {
	switch reason {
	case SourceOmissionSymbolicLink, SourceOmissionReparsePoint, SourceOmissionSpecialFile,
		SourceOmissionName, SourceOmissionNameCollision, SourceOmissionAccessDenied:
		return true
	default:
		return false
	}
}

func (reason SourceOmissionReason) RequiresDetail() bool {
	return reason == SourceOmissionName || reason == SourceOmissionNameCollision
}

type SourceOmission struct {
	Name   string
	Reason SourceOmissionReason
	// Detail explains the rejected spelling or colliding partner, including code points when visually identical.
	Detail string
}

func NewSiblingCollisionOmission(name, other string) SourceOmission {
	return SourceOmission{
		Name:   name,
		Reason: SourceOmissionNameCollision,
		Detail: siblingCollisionDetail(name, other),
	}
}

func siblingCollisionDetail(name, other string) string {
	if name == other {
		return fmt.Sprintf("duplicate sibling %q", other)
	}
	// Canonically equivalent spellings (such as NFC vs NFD) render identically on
	// screen; surface the differing scalar sequences so users know what to rename.
	if unicode15.NormalizeNFC(name) == unicode15.NormalizeNFC(other) {
		leftDiff, rightDiff := differingRuneSpan([]rune(name), []rune(other))
		return fmt.Sprintf("collides with sibling %q (%s vs %s)", other, formatScalarSpan(leftDiff), formatScalarSpan(rightDiff))
	}
	return fmt.Sprintf("collides with sibling %q", other)
}

func differingRuneSpan(left, right []rune) ([]rune, []rune) {
	prefix := 0
	for prefix < len(left) && prefix < len(right) && left[prefix] == right[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(left)-prefix && suffix < len(right)-prefix &&
		left[len(left)-1-suffix] == right[len(right)-1-suffix] {
		suffix++
	}
	leftSpan := left[prefix : len(left)-suffix]
	rightSpan := right[prefix : len(right)-suffix]
	if len(leftSpan) == 0 || len(rightSpan) == 0 {
		return left, right
	}
	return leftSpan, rightSpan
}

func formatScalarSpan(scalars []rune) string {
	limit := min(len(scalars), maxCollisionScalarPreview)
	parts := make([]string, 0, limit+1)
	for index := range limit {
		parts = append(parts, fmt.Sprintf("%U", scalars[index]))
	}
	if len(scalars) > limit {
		parts = append(parts, "...")
	}
	return strings.Join(parts, " ")
}

// ScanOmissionReporter publishes the complete omission summary for one
// directory scan once catalog-level admission and commit succeed. Keeping
// publication on the source preserves native paths and diagnostic correlation
// without exposing host filesystem layout to catalog storage.
type ScanOmissionReporter interface {
	ReportScanOmissions(additionalCount uint64, additionalSamples []SourceOmission)
}

type ScanOmissionReporterFunc func(additionalCount uint64, additionalSamples []SourceOmission)

func (report ScanOmissionReporterFunc) ReportScanOmissions(additionalCount uint64, additionalSamples []SourceOmission) {
	if report != nil {
		report(additionalCount, additionalSamples)
	}
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

// admitSortedSiblings withholds every sibling group whose names collide under
// portable matching and returns the memory bytes no longer retained after
// filtering (omitted records plus any transient pre-NFC raw spellings).
func admitSortedSiblings(items []scannedNode) ([]NodeRecord, uint64, []SourceOmission, uint64) {
	if len(items) == 0 {
		return nil, 0, nil, 0
	}
	firstByKey := make(map[string]int, len(items))
	var collidingGroups map[string][]int
	var colliding []bool
	for index, item := range items {
		key := siblingCollisionKey(item.record.Entry().Name())
		first, exists := firstByKey[key]
		if !exists {
			firstByKey[key] = index
			continue
		}
		if collidingGroups == nil {
			collidingGroups = make(map[string][]int)
			colliding = make([]bool, len(items))
		}
		if !colliding[first] {
			colliding[first] = true
			collidingGroups[key] = append(collidingGroups[key], first)
		}
		colliding[index] = true
		collidingGroups[key] = append(collidingGroups[key], index)
	}

	var releasedBytes uint64
	if len(collidingGroups) == 0 {
		admitted := make([]NodeRecord, len(items))
		for index, item := range items {
			admitted[index] = item.record
			releasedBytes += item.extra
		}
		return admitted, 0, nil, releasedBytes
	}

	keys := make([]string, 0, len(collidingGroups))
	for key := range collidingGroups {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var omittedCount uint64
	var samples []SourceOmission
	for _, key := range keys {
		members := collidingGroups[key]
		slices.SortFunc(members, func(left, right int) int {
			return strings.Compare(items[left].rawName, items[right].rawName)
		})
		omittedCount += uint64(len(members))
		if len(samples) >= MaxSourceOmissionSamples {
			continue
		}
		firstName := items[members[0]].rawName
		secondName := items[members[1]].rawName
		samples = append(samples, NewSiblingCollisionOmission(firstName, secondName))
		for _, member := range members[1:] {
			if len(samples) >= MaxSourceOmissionSamples {
				break
			}
			samples = append(samples, NewSiblingCollisionOmission(items[member].rawName, firstName))
		}
	}

	admitted := make([]NodeRecord, 0, len(items)-int(omittedCount))
	for index, item := range items {
		releasedBytes += item.extra
		if colliding[index] {
			releasedBytes += item.record.EstimatedMemoryBytes()
			continue
		}
		admitted = append(admitted, item.record)
	}
	return admitted, omittedCount, samples, releasedBytes
}
