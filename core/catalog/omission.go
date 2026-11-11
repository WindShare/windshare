package catalog

import (
	"bytes"
	"context"
	"fmt"
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

type candidateRecord struct {
	collisionKey []byte
	rawName      []byte
	payload      []byte
}

func nextCandidateRecord(ctx context.Context, cursor *sortCursor) (candidateRecord, bool, error) {
	record, ok, err := cursor.next(ctx)
	if err != nil || !ok {
		return candidateRecord{}, false, err
	}
	collisionKey, rawName, valid := splitCandidateKey(record.key)
	if !valid {
		return candidateRecord{}, false, fmt.Errorf("%w: malformed candidate sort key", ErrCorruptCatalogStorage)
	}
	return candidateRecord{
		collisionKey: collisionKey,
		rawName:      rawName,
		payload:      record.payload,
	}, true, nil
}

type pendingCandidate struct {
	collisionKey []byte
	rawName      []byte
	payload      []byte
	lease        *BudgetReservation
}

func newPendingCandidate(record candidateRecord, hierarchy BudgetHierarchy) (pendingCandidate, error) {
	memory := sortRecordMemoryOverhead + uint64(len(record.collisionKey)+len(record.rawName)+len(record.payload))
	lease, err := ReserveHierarchy(hierarchy, ResourceUsage{MemoryBytes: memory})
	if err != nil {
		return pendingCandidate{}, err
	}
	return pendingCandidate{
		collisionKey: append([]byte(nil), record.collisionKey...),
		rawName:      append([]byte(nil), record.rawName...),
		payload:      append([]byte(nil), record.payload...),
		lease:        lease,
	}, nil
}

func (p *pendingCandidate) release() {
	if p.lease != nil {
		p.lease.Release()
		p.lease = nil
	}
	p.collisionKey = nil
	p.rawName = nil
	p.payload = nil
}

func splitCandidateKey(key []byte) (collisionKey, rawName []byte, ok bool) {
	return bytes.Cut(key, []byte{0})
}

type collisionOmissionCollector struct {
	count   uint64
	samples []SourceOmission
}

func (c *collisionOmissionCollector) recordPair(firstName, secondName string) {
	c.count += 2
	c.appendSample(firstName, secondName)
	c.appendSample(secondName, firstName)
}

func (c *collisionOmissionCollector) recordFollower(followerName, firstName string) {
	c.count++
	c.appendSample(followerName, firstName)
}

func (c *collisionOmissionCollector) appendSample(name, other string) {
	if len(c.samples) < MaxSourceOmissionSamples {
		c.samples = append(c.samples, NewSiblingCollisionOmission(name, other))
	}
}

func drainCollisionGroup(
	ctx context.Context,
	cursor *sortCursor,
	hierarchy BudgetHierarchy,
	groupKey []byte,
	firstName string,
	collector *collisionOmissionCollector,
) (pendingCandidate, bool, error) {
	for {
		follower, ok, err := nextCandidateRecord(ctx, cursor)
		if err != nil || !ok {
			return pendingCandidate{}, false, err
		}
		if !bytes.Equal(groupKey, follower.collisionKey) {
			nextPending, err := newPendingCandidate(follower, hierarchy)
			return nextPending, err == nil, err
		}
		collector.recordFollower(string(follower.rawName), firstName)
	}
}

func (s *directorySorter) emitCandidate(ctx context.Context, candidate *pendingCandidate) error {
	canonicalName := unicode15.NormalizeNFC(string(candidate.rawName))
	return s.nodes.add(ctx, sortRecord{key: []byte(canonicalName), payload: candidate.payload})
}

func (s *directorySorter) advanceCandidate(ctx context.Context, pending *pendingCandidate, next candidateRecord) error {
	if err := s.emitCandidate(ctx, pending); err != nil {
		return err
	}
	pending.release()
	replacement, err := newPendingCandidate(next, s.hierarchy)
	if err != nil {
		return err
	}
	*pending = replacement
	return nil
}

func (s *directorySorter) partitionCandidates(
	ctx context.Context,
	candidates SpillObject,
) (uint64, []SourceOmission, error) {
	cursor, err := newSortCursor(ctx, candidates, s.hierarchy)
	if err != nil {
		return 0, nil, err
	}
	defer cursor.close()

	first, ok, err := nextCandidateRecord(ctx, cursor)
	if err != nil || !ok {
		return 0, nil, err
	}
	pending, err := newPendingCandidate(first, s.hierarchy)
	if err != nil {
		return 0, nil, err
	}
	defer pending.release()

	var collector collisionOmissionCollector
	for {
		next, nextOK, err := nextCandidateRecord(ctx, cursor)
		if err != nil {
			return 0, nil, err
		}
		if !nextOK {
			if err := s.emitCandidate(ctx, &pending); err != nil {
				return 0, nil, err
			}
			pending.release()
			return collector.count, collector.samples, nil
		}
		if !bytes.Equal(pending.collisionKey, next.collisionKey) {
			if err := s.advanceCandidate(ctx, &pending, next); err != nil {
				return 0, nil, err
			}
			continue
		}

		// A collision group of size >= 2 withholds every member. Release the pending
		// payload immediately so even an adversarial group of 1M case-variants retains
		// only the group's first name while draining remaining followers.
		firstName := string(pending.rawName)
		groupKey := append([]byte(nil), pending.collisionKey...)
		pending.release()
		collector.recordPair(firstName, string(next.rawName))

		var hasNext bool
		pending, hasNext, err = drainCollisionGroup(ctx, cursor, s.hierarchy, groupKey, firstName, &collector)
		if err != nil {
			return 0, nil, err
		}
		if !hasNext {
			return collector.count, collector.samples, nil
		}
	}
}
