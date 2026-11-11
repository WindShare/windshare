package catalog

import (
	"bytes"
	"context"
	"errors"
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

const (
	admissionIdentityKey byte = iota
	admissionNameKey
)

type candidateRecord struct {
	collisionKey []byte
	rawName      []byte
}

func candidateFromRecord(record sortRecord) (candidateRecord, error) {
	if len(record.key) < 2 || record.key[0] != admissionNameKey || len(record.payload) != 0 {
		return candidateRecord{}, fmt.Errorf("%w: malformed sibling admission record", ErrCorruptCatalogStorage)
	}
	collisionKey, rawName, valid := bytes.Cut(record.key[1:], []byte{0})
	if !valid || len(collisionKey) == 0 || len(rawName) == 0 {
		return candidateRecord{}, fmt.Errorf("%w: malformed candidate sort key", ErrCorruptCatalogStorage)
	}
	return candidateRecord{collisionKey: collisionKey, rawName: rawName}, nil
}

func nextCandidateRecord(ctx context.Context, cursor *sortCursor) (candidateRecord, bool, error) {
	record, ok, err := cursor.next(ctx)
	if err != nil || !ok {
		return candidateRecord{}, false, err
	}
	candidate, err := candidateFromRecord(record)
	return candidate, err == nil, err
}

// Identities sort before names so duplicate IDs fail before any collision group
// can be admitted as an omission. A single lightweight index bounds both checks.
func firstCandidateRecord(ctx context.Context, cursor *sortCursor) (candidateRecord, bool, error) {
	var previous [IdentityBytes]byte
	hasPrevious := false
	for {
		record, ok, err := cursor.next(ctx)
		if err != nil || !ok {
			return candidateRecord{}, false, err
		}
		if len(record.key) == 0 || record.key[0] != admissionIdentityKey {
			candidate, err := candidateFromRecord(record)
			return candidate, err == nil, err
		}
		if len(record.key) != IdentityBytes+1 || len(record.payload) != 0 {
			return candidateRecord{}, false, fmt.Errorf("%w: malformed node identity sort key", ErrCorruptCatalogStorage)
		}
		if hasPrevious && bytes.Equal(previous[:], record.key[1:]) {
			return candidateRecord{}, false, ErrSiblingCollision
		}
		copy(previous[:], record.key[1:])
		hasPrevious = true
	}
}

type pendingCandidate struct {
	collisionKey []byte
	rawName      []byte
	lease        *BudgetReservation
}

func retainCandidate(record candidateRecord, cursor *sortCursor) pendingCandidate {
	// Transfer the current cursor reservation with its bytes. Copying the name
	// and reserving it again would charge and allocate twice for every sibling.
	pending := pendingCandidate{
		collisionKey: record.collisionKey,
		rawName:      record.rawName,
		lease:        cursor.lease,
	}
	cursor.lease = nil
	return pending
}

func (p *pendingCandidate) release() {
	if p.lease != nil {
		p.lease.Release()
		p.lease = nil
	}
	p.collisionKey = nil
	p.rawName = nil
}

type collisionOmissionCollector struct {
	names   *externalSorter
	count   uint64
	samples []SourceOmission
}

func (c *collisionOmissionCollector) record(ctx context.Context, name, other string) error {
	canonical := unicode15.NormalizeNFC(name)
	if err := c.names.add(ctx, sortRecord{key: []byte(canonical)}); err != nil {
		return err
	}
	c.count++
	if len(c.samples) < MaxSourceOmissionSamples {
		c.samples = append(c.samples, NewSiblingCollisionOmission(name, other))
	}
	return nil
}

func (s *directorySorter) collectSiblingOmissions(
	ctx context.Context,
	admission SpillObject,
) (uint64, []SourceOmission, error) {
	cursor, err := newSortCursor(ctx, admission, s.hierarchy)
	if err != nil {
		return 0, nil, err
	}
	defer cursor.close()

	first, ok, err := firstCandidateRecord(ctx, cursor)
	if err != nil || !ok {
		return 0, nil, err
	}
	pending := retainCandidate(first, cursor)
	defer pending.release()

	collector := collisionOmissionCollector{names: s.omittedNames}
	colliding := false
	for {
		next, nextOK, err := nextCandidateRecord(ctx, cursor)
		if err != nil {
			return 0, nil, err
		}
		if !nextOK {
			return collector.count, collector.samples, nil
		}
		if !bytes.Equal(pending.collisionKey, next.collisionKey) {
			pending.release()
			pending = retainCandidate(next, cursor)
			colliding = false
			continue
		}
		// Retain only the first name of a group, regardless of its width. Every
		// member gets an exclusion key, but node payloads never enter this pass.
		firstName, nextName := string(pending.rawName), string(next.rawName)
		if !colliding {
			if err := collector.record(ctx, firstName, nextName); err != nil {
				return 0, nil, err
			}
			colliding = true
		}
		if err := collector.record(ctx, nextName, firstName); err != nil {
			return 0, nil, err
		}
	}
}

// Filtering joins two forward-only streams. Repeated canonical names retain one
// exclusion per node, including NFC/NFD aliases, so every colliding item stays
// out of the share without buffering a group or rewriting node payloads.
type admittedNodeSource struct {
	source       sortedNodeSource
	names        SpillObject
	omittedCount uint64
}

func (s admittedNodeSource) Count() uint64 { return s.source.Count() - s.omittedCount }

func (s admittedNodeSource) Open(ctx context.Context) (NodeRecordIterator, error) {
	nodes, err := s.source.Open(ctx)
	if err != nil {
		return nil, err
	}
	names, err := newSortCursor(ctx, s.names, s.source.hierarchy)
	if err != nil {
		return nil, errors.Join(err, nodes.Close())
	}
	first, err := nextOmittedName(ctx, names)
	if err != nil {
		names.close()
		return nil, errors.Join(err, nodes.Close())
	}
	return &admittedNodeIterator{nodes: nodes, names: names, nextName: first}, nil
}

func (s admittedNodeSource) Release(meter ResourceMeter) error {
	return errors.Join(s.source.Release(meter), releaseSpillObject(s.names, meter))
}

func nextOmittedName(ctx context.Context, cursor *sortCursor) ([]byte, error) {
	record, ok, err := cursor.next(ctx)
	if err != nil || !ok {
		return nil, err
	}
	if len(record.key) == 0 || len(record.payload) != 0 {
		return nil, fmt.Errorf("%w: malformed sibling exclusion", ErrCorruptCatalogStorage)
	}
	return record.key, nil
}

type admittedNodeIterator struct {
	nodes    NodeRecordIterator
	names    *sortCursor
	nextName []byte
}

func (i *admittedNodeIterator) Next(ctx context.Context) (NodeRecord, bool, error) {
	for {
		node, ok, err := i.nodes.Next(ctx)
		if err != nil {
			return NodeRecord{}, false, err
		}
		if !ok {
			if i.nextName != nil {
				return NodeRecord{}, false, fmt.Errorf("%w: unmatched sibling exclusion", ErrCorruptCatalogStorage)
			}
			return NodeRecord{}, false, nil
		}
		if i.nextName == nil {
			return node, true, nil
		}
		switch bytes.Compare(i.nextName, []byte(node.Entry().Name())) {
		case -1:
			return NodeRecord{}, false, fmt.Errorf("%w: unmatched sibling exclusion", ErrCorruptCatalogStorage)
		case 0:
			i.nextName, err = nextOmittedName(ctx, i.names)
			if err != nil {
				return NodeRecord{}, false, err
			}
		default:
			return node, true, nil
		}
	}
}

func (i *admittedNodeIterator) Close() error {
	i.names.close()
	return i.nodes.Close()
}
