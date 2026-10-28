package checkpointstore

import (
	"bytes"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
)

var errFileExecutionAuthorityUnsettled = errors.New("file checkpoint authority has an unsettled durability result")

// One task/file has one fixed record location. Revision and object changes
// cannot create competing physical records or transfer saved ranges between them.
// The operation lease excludes processes; this mutex serializes local adapters.
type fileExecutionAuthority struct {
	mu                  sync.Mutex
	physical            map[checkpointmodel.RecordID]checkpointmodel.Record
	objects             map[checkpointmodel.ObjectID]map[checkpointmodel.RecordID]struct{}
	attention           []Attention
	reclaimedCandidates uint64
	recordLimit         int
	unsettled           bool
	// Adapters share the cache, so eviction must invalidate every inventory view.
	inventoryComplete bool
}

func newFileExecutionAuthority() *fileExecutionAuthority {
	return newFileExecutionAuthorityWithRecordLimit(checkpointmodel.MaxCheckpointRecordsPerOperation)
}
func newFileExecutionAuthorityWithRecordLimit(limit int) *fileExecutionAuthority {
	return &fileExecutionAuthority{physical: make(map[checkpointmodel.RecordID]checkpointmodel.Record), objects: make(map[checkpointmodel.ObjectID]map[checkpointmodel.RecordID]struct{}), recordLimit: limit}
}
func (a *fileExecutionAuthority) rebuild(records []checkpointmodel.Record, attention []Attention) error {
	if a == nil || a.recordLimit <= 0 {
		return checkpointmodel.ErrInvalidRecord
	}
	if len(records) > a.recordLimit {
		return fileexecution.ErrCheckpointRecordCapacity
	}
	next := newFileExecutionAuthorityWithRecordLimit(a.recordLimit)
	for _, r := range records {
		if err := next.add(r); err != nil {
			return err
		}
	}
	a.physical, a.objects = next.physical, next.objects
	a.attention = slices.Clone(attention)
	a.unsettled = false
	return nil
}
func (a *fileExecutionAuthority) requireSettled() error {
	if a == nil {
		return checkpointmodel.ErrInvalidRecord
	}
	if a.unsettled {
		return errFileExecutionAuthorityUnsettled
	}
	return nil
}
func (a *fileExecutionAuthority) markUnsettled() {
	if a != nil {
		a.unsettled = true
	}
}
func (a *fileExecutionAuthority) admitInitial(r checkpointmodel.Record) error {
	if a == nil || !checkpointmodel.InitialCandidate(r) {
		return checkpointmodel.ErrInvalidRecord
	}
	if err := a.requireSettled(); err != nil {
		return err
	}
	if len(a.physical) >= a.recordLimit {
		return fileexecution.ErrCheckpointRecordCapacity
	}
	if len(a.objects[r.OwnedObjectID()]) != 0 {
		return fileexecution.ErrCheckpointObjectClaimed
	}
	return nil
}
func (a *fileExecutionAuthority) add(r checkpointmodel.Record) error {
	if a == nil || !r.Valid() {
		return checkpointmodel.ErrInvalidRecord
	}
	if current, found := a.physical[r.RecordID()]; found {
		if !bytes.Equal(current.CanonicalBytes(), r.CanonicalBytes()) {
			return checkpointmodel.ErrRecordBinding
		}
		return nil
	}
	if a.recordLimit <= 0 || len(a.physical) >= a.recordLimit {
		return fileexecution.ErrCheckpointRecordCapacity
	}
	a.physical[r.RecordID()] = r
	claims := a.objects[r.OwnedObjectID()]
	if claims == nil {
		claims = make(map[checkpointmodel.RecordID]struct{})
		a.objects[r.OwnedObjectID()] = claims
	}
	claims[r.RecordID()] = struct{}{}
	return nil
}
func (a *fileExecutionAuthority) replace(previous, next checkpointmodel.Record) error {
	if a == nil || !previous.Valid() || !next.Valid() || !checkpointmodel.IdentityEqual(previous, next) {
		return checkpointmodel.ErrRecordBinding
	}
	current, found := a.physical[previous.RecordID()]
	if !found {
		return checkpointmodel.ErrRecordBinding
	}
	if bytes.Equal(current.CanonicalBytes(), next.CanonicalBytes()) {
		return nil
	}
	if !bytes.Equal(current.CanonicalBytes(), previous.CanonicalBytes()) {
		return checkpointmodel.ErrRecordBinding
	}
	a.physical[next.RecordID()] = next
	return nil
}
func (a *fileExecutionAuthority) remove(r checkpointmodel.Record) error {
	if a == nil || !r.Valid() {
		return checkpointmodel.ErrInvalidRecord
	}
	current, found := a.physical[r.RecordID()]
	if !found || !bytes.Equal(current.CanonicalBytes(), r.CanonicalBytes()) {
		return checkpointmodel.ErrRecordBinding
	}
	delete(a.physical, r.RecordID())
	claims := a.objects[r.OwnedObjectID()]
	delete(claims, r.RecordID())
	if len(claims) == 0 {
		delete(a.objects, r.OwnedObjectID())
	}
	return nil
}
func (a *fileExecutionAuthority) classify(spec checkpointmodel.CheckpointLineageSpec, request checkpointmodel.CheckpointLineageRequest) (checkpointmodel.CheckpointLineageDecision, checkpointmodel.Record, error) {
	if err := a.requireSettled(); err != nil {
		return checkpointmodel.CheckpointLineageDecisionInvalid, checkpointmodel.Record{}, err
	}
	id, err := checkpointmodel.DeriveCheckpointLineageID(spec)
	if err != nil {
		return checkpointmodel.CheckpointLineageDecisionInvalid, checkpointmodel.Record{}, err
	}
	r, found := a.physical[checkpointmodel.RecordID(id)]
	if !found {
		return checkpointmodel.ClassifyCheckpointLineage(request, nil, false), checkpointmodel.Record{}, nil
	}
	actual, err := r.CheckpointLineageSpec()
	if err != nil || !checkpointmodel.SameCheckpointLineageSpec(actual, spec) {
		return checkpointmodel.CheckpointLineageDecisionInvalid, checkpointmodel.Record{}, checkpointmodel.ErrRecordBinding
	}
	decision := checkpointmodel.ClassifyCheckpointLineage(request, []checkpointmodel.CheckpointLineageEvidence{{
		FileRevision: r.FileRevision(), ExactSize: r.ExactSize(), OwnedObjectID: r.OwnedObjectID(),
	}}, len(a.objects[r.OwnedObjectID()]) > 1)
	if decision != checkpointmodel.CheckpointLineageDecisionExact {
		return decision, checkpointmodel.Record{}, nil
	}
	return decision, r, nil
}

// Inventory is a management operation; ordinary continuation uses exact lookup.
type FileExecutionLineageSlot struct {
	decision checkpointmodel.CheckpointLineageDecision
	path     string
	records  []checkpointmodel.Record
	selected checkpointmodel.Record
}

func (s FileExecutionLineageSlot) Decision() checkpointmodel.CheckpointLineageDecision {
	return s.decision
}
func (s FileExecutionLineageSlot) CanonicalPath() string { return s.path }
func (s FileExecutionLineageSlot) Record() (checkpointmodel.Record, bool) {
	return s.selected, s.decision == checkpointmodel.CheckpointLineageDecisionExact && s.selected.Valid()
}
func (s FileExecutionLineageSlot) PhysicalRecords() []checkpointmodel.Record {
	return slices.Clone(s.records)
}
func (a *fileExecutionAuthority) lineageSnapshot() []FileExecutionLineageSlot {
	records := make([]checkpointmodel.Record, 0, len(a.physical))
	for _, r := range a.physical {
		records = append(records, r)
	}
	slices.SortFunc(records, compareRecordID)
	slots := make([]FileExecutionLineageSlot, 0, len(records))
	for _, r := range records {
		spec, _ := r.CheckpointLineageSpec()
		decision, selected, _ := a.classify(spec, checkpointmodel.CheckpointLineageRequest{FileRevision: r.FileRevision(), ExactSize: r.ExactSize()})
		slots = append(slots, FileExecutionLineageSlot{decision: decision, path: r.CanonicalPath(), records: []checkpointmodel.Record{r}, selected: selected})
	}
	return slots
}
