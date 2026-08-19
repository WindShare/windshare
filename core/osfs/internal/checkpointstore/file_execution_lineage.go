package checkpointstore

import (
	"bytes"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
)

var errFileExecutionAuthorityUnsettled = errors.New("file checkpoint authority has an unsettled durability result")

// fileExecutionAuthority is shared by every adapter opened through one held
// operation lease. The lease excludes other processes; this mutex closes the
// remaining goroutine race between classifying a logical slot and installing
// its first physical RecordID.
type fileExecutionAuthority struct {
	mu sync.Mutex

	physical    map[checkpointmodel.RecordID]checkpointmodel.Record
	lineages    map[checkpointmodel.CheckpointLineageID][]*fileExecutionLineageBucket
	objects     map[checkpointmodel.ObjectID]map[checkpointmodel.RecordID]struct{}
	attention   []Attention
	recordLimit int
	unsettled   bool
}

type fileExecutionLineageBucket struct {
	id      checkpointmodel.CheckpointLineageID
	spec    checkpointmodel.CheckpointLineageSpec
	records map[checkpointmodel.RecordID]checkpointmodel.Record
}

func newFileExecutionAuthority() *fileExecutionAuthority {
	return newFileExecutionAuthorityWithRecordLimit(checkpointmodel.MaxCheckpointRecordsPerOperation)
}

func newFileExecutionAuthorityWithRecordLimit(recordLimit int) *fileExecutionAuthority {
	return &fileExecutionAuthority{
		physical:    make(map[checkpointmodel.RecordID]checkpointmodel.Record),
		lineages:    make(map[checkpointmodel.CheckpointLineageID][]*fileExecutionLineageBucket),
		objects:     make(map[checkpointmodel.ObjectID]map[checkpointmodel.RecordID]struct{}),
		recordLimit: recordLimit,
	}
}

func (authority *fileExecutionAuthority) rebuild(records []checkpointmodel.Record, attention []Attention) error {
	if authority == nil || authority.recordLimit <= 0 || len(records) > authority.recordLimit {
		if authority != nil && authority.recordLimit > 0 && len(records) > authority.recordLimit {
			return fileexecution.ErrCheckpointRecordCapacity
		}
		return checkpointmodel.ErrInvalidRecord
	}
	authority.physical = make(map[checkpointmodel.RecordID]checkpointmodel.Record, len(records))
	authority.lineages = make(map[checkpointmodel.CheckpointLineageID][]*fileExecutionLineageBucket)
	authority.objects = make(map[checkpointmodel.ObjectID]map[checkpointmodel.RecordID]struct{})
	authority.attention = slices.Clone(attention)
	authority.unsettled = false
	for _, record := range records {
		if err := authority.add(record); err != nil {
			return err
		}
	}
	return nil
}

// admitInitial validates proposal-only constraints while the operation-wide
// authority mutex is held. A proposed object is allocation input, not lineage
// evidence, so rejection cannot poison either logical slot.
func (authority *fileExecutionAuthority) admitInitial(candidate checkpointmodel.Record) error {
	if authority == nil || !checkpointmodel.InitialCandidate(candidate) {
		return checkpointmodel.ErrInvalidRecord
	}
	if authority.unsettled {
		return errFileExecutionAuthorityUnsettled
	}
	if len(authority.physical) >= authority.recordLimit {
		return fileexecution.ErrCheckpointRecordCapacity
	}
	if len(authority.objects[candidate.OwnedObjectID()]) != 0 {
		return fileexecution.ErrCheckpointObjectClaimed
	}
	return nil
}

func (authority *fileExecutionAuthority) markUnsettled() {
	if authority != nil {
		authority.unsettled = true
	}
}

func (authority *fileExecutionAuthority) requireSettled() error {
	if authority == nil {
		return checkpointmodel.ErrInvalidRecord
	}
	if authority.unsettled {
		return errFileExecutionAuthorityUnsettled
	}
	return nil
}

func (authority *fileExecutionAuthority) add(record checkpointmodel.Record) error {
	if authority == nil || !record.Valid() {
		return checkpointmodel.ErrInvalidRecord
	}
	recordID := record.RecordID()
	if current, exists := authority.physical[recordID]; exists {
		if !bytes.Equal(current.CanonicalBytes(), record.CanonicalBytes()) {
			return checkpointmodel.ErrRecordBinding
		}
		return nil
	}
	if authority.recordLimit <= 0 || len(authority.physical) >= authority.recordLimit {
		return fileexecution.ErrCheckpointRecordCapacity
	}
	spec, err := record.CheckpointLineageSpec()
	if err != nil {
		return err
	}
	lineageID, err := record.CheckpointLineageID()
	if err != nil {
		return err
	}
	bucket := authority.findBucket(lineageID, spec)
	if bucket == nil {
		bucket = &fileExecutionLineageBucket{
			id: lineageID, spec: spec,
			records: make(map[checkpointmodel.RecordID]checkpointmodel.Record),
		}
		authority.lineages[lineageID] = append(authority.lineages[lineageID], bucket)
	}
	bucket.records[recordID] = record
	authority.physical[recordID] = record
	claims := authority.objects[record.OwnedObjectID()]
	if claims == nil {
		claims = make(map[checkpointmodel.RecordID]struct{})
		authority.objects[record.OwnedObjectID()] = claims
	}
	claims[recordID] = struct{}{}
	return nil
}

func (authority *fileExecutionAuthority) replace(previous, next checkpointmodel.Record) error {
	if authority == nil || !previous.Valid() || !next.Valid() || previous.RecordID() != next.RecordID() {
		return checkpointmodel.ErrRecordBinding
	}
	current, found := authority.physical[previous.RecordID()]
	if !found {
		return checkpointmodel.ErrRecordBinding
	}
	if bytes.Equal(current.CanonicalBytes(), next.CanonicalBytes()) {
		return nil
	}
	if !bytes.Equal(current.CanonicalBytes(), previous.CanonicalBytes()) ||
		!checkpointmodel.SameCheckpointLineage(previous, next) ||
		previous.OwnedObjectID() != next.OwnedObjectID() {
		return checkpointmodel.ErrRecordBinding
	}
	bucket, err := authority.bucketForRecord(previous)
	if err != nil {
		return err
	}
	bucket.records[next.RecordID()] = next
	authority.physical[next.RecordID()] = next
	return nil
}

func (authority *fileExecutionAuthority) remove(record checkpointmodel.Record) error {
	if authority == nil || !record.Valid() {
		return checkpointmodel.ErrInvalidRecord
	}
	current, found := authority.physical[record.RecordID()]
	if !found || !bytes.Equal(current.CanonicalBytes(), record.CanonicalBytes()) {
		return checkpointmodel.ErrRecordBinding
	}
	bucket, err := authority.bucketForRecord(record)
	if err != nil {
		return err
	}
	delete(bucket.records, record.RecordID())
	delete(authority.physical, record.RecordID())
	claims := authority.objects[record.OwnedObjectID()]
	delete(claims, record.RecordID())
	if len(claims) == 0 {
		delete(authority.objects, record.OwnedObjectID())
	}
	if len(bucket.records) == 0 {
		buckets := authority.lineages[bucket.id]
		for index, current := range buckets {
			if current == bucket {
				authority.lineages[bucket.id] = slices.Delete(buckets, index, index+1)
				break
			}
		}
		if len(authority.lineages[bucket.id]) == 0 {
			delete(authority.lineages, bucket.id)
		}
	}
	return nil
}

func (authority *fileExecutionAuthority) classify(
	spec checkpointmodel.CheckpointLineageSpec,
	request checkpointmodel.CheckpointLineageRequest,
) (checkpointmodel.CheckpointLineageDecision, checkpointmodel.Record, error) {
	if err := authority.requireSettled(); err != nil {
		return checkpointmodel.CheckpointLineageDecisionInvalid, checkpointmodel.Record{}, err
	}
	lineageID, err := checkpointmodel.DeriveCheckpointLineageID(spec)
	if err != nil {
		return checkpointmodel.CheckpointLineageDecisionInvalid, checkpointmodel.Record{}, err
	}
	bucket := authority.findBucket(lineageID, spec)
	if bucket == nil {
		return checkpointmodel.ClassifyCheckpointLineage(request, nil, false), checkpointmodel.Record{}, nil
	}
	records := bucket.sortedRecords()
	evidence := make([]checkpointmodel.CheckpointLineageEvidence, len(records))
	for index, record := range records {
		evidence[index] = checkpointmodel.CheckpointLineageEvidence{
			FileRevision: record.FileRevision(), ExactSize: record.ExactSize(),
			OwnedObjectID: record.OwnedObjectID(),
		}
	}
	decision := checkpointmodel.ClassifyCheckpointLineage(
		request, evidence, authority.crossLineageObjectConflict(bucket),
	)
	if decision != checkpointmodel.CheckpointLineageDecisionExact {
		return decision, checkpointmodel.Record{}, nil
	}
	for _, record := range records {
		if record.FileRevision() == request.FileRevision && record.ExactSize() == request.ExactSize {
			return decision, record, nil
		}
	}
	return checkpointmodel.CheckpointLineageDecisionInvalid, checkpointmodel.Record{}, checkpointmodel.ErrRecordBinding
}

func (authority *fileExecutionAuthority) intrinsicDecision(
	bucket *fileExecutionLineageBucket,
) (checkpointmodel.CheckpointLineageDecision, checkpointmodel.Record) {
	records := bucket.sortedRecords()
	if len(records) == 0 {
		return checkpointmodel.CheckpointLineageDecisionAbsent, checkpointmodel.Record{}
	}
	request := checkpointmodel.CheckpointLineageRequest{
		FileRevision: records[0].FileRevision(), ExactSize: records[0].ExactSize(),
	}
	decision, record, err := authority.classify(bucket.spec, request)
	if err != nil {
		return checkpointmodel.CheckpointLineageDecisionInvalid, checkpointmodel.Record{}
	}
	return decision, record
}

func (authority *fileExecutionAuthority) findBucket(
	lineageID checkpointmodel.CheckpointLineageID,
	spec checkpointmodel.CheckpointLineageSpec,
) *fileExecutionLineageBucket {
	for _, bucket := range authority.lineages[lineageID] {
		if checkpointmodel.SameCheckpointLineageSpec(bucket.spec, spec) {
			return bucket
		}
	}
	return nil
}

func (authority *fileExecutionAuthority) bucketForRecord(
	record checkpointmodel.Record,
) (*fileExecutionLineageBucket, error) {
	spec, err := record.CheckpointLineageSpec()
	if err != nil {
		return nil, err
	}
	lineageID, err := record.CheckpointLineageID()
	if err != nil {
		return nil, err
	}
	bucket := authority.findBucket(lineageID, spec)
	if bucket == nil {
		return nil, checkpointmodel.ErrRecordBinding
	}
	return bucket, nil
}

func (authority *fileExecutionAuthority) crossLineageObjectConflict(
	bucket *fileExecutionLineageBucket,
) bool {
	for _, record := range bucket.records {
		for recordID := range authority.objects[record.OwnedObjectID()] {
			claimed := authority.physical[recordID]
			if claimed.Valid() && !checkpointmodel.SameCheckpointLineage(record, claimed) {
				return true
			}
		}
	}
	return false
}

func (bucket *fileExecutionLineageBucket) sortedRecords() []checkpointmodel.Record {
	records := make([]checkpointmodel.Record, 0, len(bucket.records))
	for _, record := range bucket.records {
		records = append(records, record)
	}
	slices.SortFunc(records, func(left, right checkpointmodel.Record) int {
		return bytes.Compare(left.RecordID().Bytes(), right.RecordID().Bytes())
	})
	return records
}

// FileExecutionLineageSlot is a read-only logical inventory entry. Conflicts
// retain their physical records for explicit cleanup, while only Exact exposes a
// selected range authority.
type FileExecutionLineageSlot struct {
	decision checkpointmodel.CheckpointLineageDecision
	path     string
	records  []checkpointmodel.Record
	selected checkpointmodel.Record
}

func (slot FileExecutionLineageSlot) Decision() checkpointmodel.CheckpointLineageDecision {
	return slot.decision
}

func (slot FileExecutionLineageSlot) CanonicalPath() string { return slot.path }

func (slot FileExecutionLineageSlot) Record() (checkpointmodel.Record, bool) {
	return slot.selected, slot.decision == checkpointmodel.CheckpointLineageDecisionExact && slot.selected.Valid()
}

func (slot FileExecutionLineageSlot) PhysicalRecords() []checkpointmodel.Record {
	return slices.Clone(slot.records)
}

func (authority *fileExecutionAuthority) lineageSnapshot() []FileExecutionLineageSlot {
	type orderedBucket struct {
		bucket    *fileExecutionLineageBucket
		canonical []byte
	}
	ordered := make([]orderedBucket, 0, len(authority.physical))
	for _, buckets := range authority.lineages {
		for _, bucket := range buckets {
			canonical, _ := checkpointmodel.CanonicalCheckpointLineageBytes(bucket.spec)
			ordered = append(ordered, orderedBucket{bucket: bucket, canonical: canonical})
		}
	}
	slices.SortFunc(ordered, func(left, right orderedBucket) int {
		if compared := bytes.Compare(left.bucket.id.Bytes(), right.bucket.id.Bytes()); compared != 0 {
			return compared
		}
		return bytes.Compare(left.canonical, right.canonical)
	})
	result := make([]FileExecutionLineageSlot, 0, len(ordered))
	for _, current := range ordered {
		decision, selected := authority.intrinsicDecision(current.bucket)
		result = append(result, FileExecutionLineageSlot{
			decision: decision, path: current.bucket.spec.CanonicalPath,
			records: current.bucket.sortedRecords(), selected: selected,
		})
	}
	return result
}
