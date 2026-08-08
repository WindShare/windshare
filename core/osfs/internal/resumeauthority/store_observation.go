package resumeauthority

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

type resumeLeasedRepository struct {
	mu sync.Mutex

	lease         resumeIntentLease
	root          *resumeRootPins
	intentName    string
	intentPin     outputcap.CurrentEntryReference
	intent        outputcap.Directory
	binding       checkpointmodel.Binding
	baseEvidence  Evidence
	baseAttention []Attention

	records *resumeOwnedDirectory
	anchors *resumeOwnedDirectory
	stages  *resumeOwnedDirectory

	checkpointPins map[checkpointmodel.RecordID]*resumeCheckpointPins
	artifactPins   []*resumeArtifactPins
	expected       []resumeExpectedAction
	nextAction     int
	applyAttention []Attention
	snapshot       RepositorySnapshot
	observed       bool
	closed         bool
	closeErr       error
}

type resumeIntentLease interface {
	Binding() checkpointmodel.Binding
	Close() error
}

type resumeOwnedDirectory struct {
	name      string
	pin       outputcap.CurrentEntryReference
	directory outputcap.Directory
	shards    map[string]*resumeShardPins
}

type resumeShardPins struct {
	owner     *resumeOwnedDirectory
	name      string
	pin       outputcap.CurrentEntryReference
	directory outputcap.Directory
}

type resumeEntryPins struct {
	shard *resumeShardPins
	name  string
	pin   outputcap.CurrentEntryReference
}

type resumeArtifactPins struct {
	object checkpointmodel.ObjectID
	entry  resumeEntryPins
}

type resumeCheckpointPins struct {
	record  checkpointmodel.Record
	encoded []byte
	entry   resumeEntryPins
	stage   *resumeArtifactPins
	anchor  *resumeArtifactPins
}

type resumeExpectedAction struct {
	kind     ActionKind
	recordID checkpointmodel.RecordID
}

func acquireResumeIntent(
	ctx context.Context,
	config checkpointstore.CertifiedConfig,
	listedRoot *resumeRootPins,
	item resumeInventoryItem,
) (LeasedRepository, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	// AcquireIntent creates/synchronizes the deterministic lock carrier before
	// this function revalidates or enumerates any selected-intent child.
	lease, err := listedRoot.namespace.AcquireIntent(item.intent)
	if err != nil {
		return nil, projectResumeError("acquire selected intent lease", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	closeLeaseOnError := func(operationErr error) (LeasedRepository, error) {
		return nil, errors.Join(operationErr, projectResumeError("release failed intent lease", lease.Close()))
	}

	currentRoot, currentAttention, err := openResumeRoot(config)
	if err != nil {
		reason, attentionState := resumeOpenAttention(err)
		if !attentionState {
			return closeLeaseOnError(projectResumeError("reopen leased namespace", err))
		}
		return &resumeLeasedRepository{
			lease: &lease, binding: lease.Binding(), baseEvidence: resumeEvidenceForOpenError(err),
			baseAttention: append(slices.Clone(item.attention),
				resumeAdapterAttention(reason, item.intent.Bytes())),
		}, nil
	}
	if err := contextErr(ctx); err != nil {
		return nil, errors.Join(err, currentRoot.Close(), lease.Close())
	}
	same, err := currentRoot.sameLineage(listedRoot)
	if err != nil {
		return nil, errors.Join(
			projectResumeError("revalidate selected namespace lineage", err),
			currentRoot.Close(), lease.Close(),
		)
	}
	leased := &resumeLeasedRepository{
		lease: &lease, root: currentRoot, intentName: item.name, binding: lease.Binding(),
		baseEvidence:   EvidenceExact,
		baseAttention:  append(slices.Clone(item.attention), currentAttention...),
		checkpointPins: make(map[checkpointmodel.RecordID]*resumeCheckpointPins),
	}
	if !same {
		leased.baseEvidence = EvidenceReplaced
		return leased, nil
	}
	evidence, err := pinnedEntryEvidence(currentRoot.intents, item.name, item.pin)
	if err != nil {
		return nil, errors.Join(
			projectResumeError("revalidate selected intent pin", err), leased.Close(),
		)
	}
	if evidence != EvidenceExact {
		leased.baseEvidence = evidence
		return leased, nil
	}
	leased.intentPin, leased.intent, err =
		pinExistingDirectory(currentRoot.intents, item.name)
	if err != nil {
		if reason, attentionState := resumeOpenAttention(err); attentionState {
			leased.baseEvidence = resumeEvidenceForOpenError(err)
			leased.baseAttention = append(leased.baseAttention,
				resumeAdapterAttention(reason, item.intent.Bytes()))
			return leased, nil
		}
		return nil, errors.Join(
			projectResumeError("pin selected intent", err), leased.Close(),
		)
	}
	return leased, nil
}

func resumeEvidenceForOpenError(err error) Evidence {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return EvidenceAbsent
	case errors.Is(err, errResumePinReplaced):
		return EvidenceReplaced
	default:
		return EvidenceAmbiguous
	}
}

func (repository *resumeLeasedRepository) Observe(
	ctx context.Context,
) (RepositorySnapshot, error) {
	if err := contextErr(ctx); err != nil {
		return RepositorySnapshot{}, err
	}
	if repository == nil {
		return RepositorySnapshot{},
			projectResumeError("observe resume repository", transfer.ErrInvalidOutputBinding)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.closed {
		return RepositorySnapshot{},
			projectResumeError("observe closed resume repository", transfer.ErrInvalidOutputBinding)
	}
	if repository.observed {
		return repository.snapshot, nil
	}

	if repository.baseEvidence != EvidenceExact {
		return repository.cacheSnapshot(repository.baseEvidence, nil, repository.baseAttention)
	}
	evidence, err := repository.revalidateNamespace()
	if err != nil {
		return RepositorySnapshot{}, projectResumeError("revalidate resume namespace", err)
	}
	if evidence != EvidenceExact {
		return repository.cacheSnapshot(evidence, nil, repository.baseAttention)
	}
	attention, err := repository.openIntentLayout(ctx)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	observations, scanAttention, err := repository.scanIntent(ctx)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	attention = append(attention, scanAttention...)
	attention = append(attention, repository.baseAttention...)
	return repository.cacheSnapshot(EvidenceExact, observations, attention)
}

func (repository *resumeLeasedRepository) cacheSnapshot(
	evidence Evidence,
	observations []CheckpointObservation,
	attention []Attention,
) (RepositorySnapshot, error) {
	snapshot, err := NewRepositorySnapshot(
		evidence, repository.binding, observations, attention,
	)
	if err != nil {
		return RepositorySnapshot{}, projectResumeError("project resume snapshot", err)
	}
	repository.snapshot = snapshot
	repository.observed = true
	return snapshot, nil
}

func (repository *resumeLeasedRepository) revalidateNamespace() (Evidence, error) {
	if repository.root == nil || repository.intent == nil || repository.intentPin == nil {
		return repository.baseEvidence, nil
	}
	evidence, err := repository.root.revalidate()
	if err != nil || evidence != EvidenceExact {
		return evidence, err
	}
	evidence, err = pinnedEntryEvidence(
		repository.root.intents, repository.intentName, repository.intentPin,
	)
	if err != nil || evidence != EvidenceExact {
		return evidence, err
	}
	exact, err := resumeIntentLayoutExact(repository.intent)
	if err != nil {
		return EvidenceAmbiguous, err
	}
	if !exact {
		return EvidenceAmbiguous, nil
	}
	return EvidenceExact, nil
}

func resumeIntentLayoutExact(intent outputcap.Directory) (bool, error) {
	if intent == nil {
		return false, transfer.ErrInvalidOutputBinding
	}
	names, err := intent.Names(checkpointstore.IntentEntryLimit() + 1)
	if err != nil {
		return false, err
	}
	if len(names) > checkpointstore.IntentEntryLimit() {
		return false, nil
	}
	for _, name := range names {
		expected, known := checkpointstore.IntentEntryKind(name)
		if !known {
			return false, nil
		}
		kind, exact, err := intent.ClassifyExactEntry(name)
		if err != nil {
			return false, err
		}
		if !exact || kind != expected {
			return false, nil
		}
	}
	return true, nil
}

func (repository *resumeLeasedRepository) openIntentLayout(
	ctx context.Context,
) ([]Attention, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	names, err := repository.intent.Names(checkpointstore.IntentEntryLimit() + 1)
	if err != nil {
		return nil, projectResumeError("list selected intent layout", err)
	}
	attention := make([]Attention, 0)
	if len(names) > checkpointstore.IntentEntryLimit() {
		attention = append(attention,
			resumeAdapterAttention(AttentionUnknownChildren, []byte("intent-overflow")))
	}
	for _, name := range names {
		expected, known := checkpointstore.IntentEntryKind(name)
		if !known {
			attention = append(attention,
				resumeAdapterAttention(AttentionUnknownChildren, []byte(name)))
			continue
		}
		kind, exact, classifyErr := repository.intent.ClassifyExactEntry(name)
		if classifyErr != nil {
			return nil, projectResumeError("classify selected intent layout", classifyErr)
		}
		if !exact || kind != expected {
			attention = append(attention,
				resumeAdapterAttention(AttentionCorruptBinding, []byte(name)))
		}
	}

	for _, target := range []struct {
		name string
		set  func(*resumeOwnedDirectory)
	}{
		{checkpointstore.RecordsDirectory, func(value *resumeOwnedDirectory) { repository.records = value }},
		{checkpointstore.AnchorsDirectory, func(value *resumeOwnedDirectory) { repository.anchors = value }},
		{checkpointstore.StagesDirectory, func(value *resumeOwnedDirectory) { repository.stages = value }},
	} {
		pin, directory, openErr := pinExistingDirectory(repository.intent, target.name)
		if openErr != nil {
			if reason, attentionState := resumeOpenAttention(openErr); attentionState {
				attention = append(attention, resumeAdapterAttention(reason, []byte(target.name)))
				continue
			}
			return nil, projectResumeError("pin selected intent directory", openErr)
		}
		target.set(&resumeOwnedDirectory{
			name: target.name, pin: pin, directory: directory,
			shards: make(map[string]*resumeShardPins),
		})
	}
	return attention, nil
}

func (repository *resumeLeasedRepository) scanIntent(
	ctx context.Context,
) ([]CheckpointObservation, []Attention, error) {
	if repository.records == nil || repository.anchors == nil || repository.stages == nil {
		return nil, nil, nil
	}
	records, recordAttention, err := repository.scanRecords(ctx)
	if err != nil {
		return nil, nil, err
	}
	stages, stageAttention, err := repository.scanArtifacts(ctx, repository.stages, checkpointstore.RecoveryStage)
	if err != nil {
		return nil, nil, err
	}
	anchors, anchorAttention, err := repository.scanArtifacts(ctx, repository.anchors, checkpointstore.RecoveryAnchor)
	if err != nil {
		return nil, nil, err
	}
	attention := slices.Concat(recordAttention, stageAttention, anchorAttention)
	observations := make([]CheckpointObservation, 0, len(records))
	ownedObjects := make(map[checkpointmodel.ObjectID]uint64, len(records))
	for _, checkpoint := range records {
		ownedObjects[checkpoint.record.OwnedOutputObject()]++
	}
	for object, owners := range ownedObjects {
		if owners > 1 {
			attention = append(attention, resumeAdapterAttention(
				AttentionCorruptBinding, object.Bytes(),
			))
		}
	}
	for object := range stages {
		if ownedObjects[object] == 0 {
			attention = append(attention, resumeAdapterAttention(
				AttentionUnknownChildren, object.Bytes(),
			))
		}
	}
	for object := range anchors {
		if ownedObjects[object] == 0 {
			attention = append(attention, resumeAdapterAttention(
				AttentionUnknownChildren, object.Bytes(),
			))
		}
	}

	recordIDs := make([]checkpointmodel.RecordID, 0, len(records))
	for recordID := range records {
		recordIDs = append(recordIDs, recordID)
	}
	slices.SortFunc(recordIDs, compareRecordIDs)
	for _, recordID := range recordIDs {
		if err := contextErr(ctx); err != nil {
			return nil, nil, err
		}
		checkpoint := records[recordID]
		object := checkpoint.record.OwnedOutputObject()
		checkpoint.stage = stages[object]
		checkpoint.anchor = anchors[object]
		stageEvidence, anchorEvidence, artifactAttention, validateErr :=
			validateResumeArtifacts(checkpoint)
		if validateErr != nil {
			return nil, nil, projectResumeError("validate owned recovery artifacts", validateErr)
		}
		attention = append(attention, artifactAttention...)
		observation, observationErr := NewCheckpointObservation(
			recordID, checkpoint.record, EvidenceExact,
			stageEvidence, anchorEvidence,
		)
		if observationErr != nil {
			return nil, nil, projectResumeError("project checkpoint observation", observationErr)
		}
		observations = append(observations, observation)
		repository.appendExpectedActions(checkpoint, stageEvidence, anchorEvidence)
	}
	return observations, attention, nil
}

func (directory *resumeOwnedDirectory) Close() error {
	if directory == nil {
		return nil
	}
	errs := make([]error, 0, len(directory.shards)*2+2)
	for _, shard := range directory.shards {
		errs = append(errs,
			closeDirectory(shard.directory), closeEntryReference(shard.pin))
	}
	errs = append(errs, closeDirectory(directory.directory), closeEntryReference(directory.pin))
	*directory = resumeOwnedDirectory{}
	return errors.Join(errs...)
}

func (repository *resumeLeasedRepository) Close() error {
	if repository == nil {
		return nil
	}
	repository.mu.Lock()
	if repository.closed {
		err := repository.closeErr
		repository.mu.Unlock()
		return err
	}
	repository.closed = true
	checkpointPins := repository.checkpointPins
	artifactPins := repository.artifactPins
	records, anchors, stages := repository.records, repository.anchors, repository.stages
	intent, intentPin := repository.intent, repository.intentPin
	root := repository.root
	lease := repository.lease

	closeErrors := make([]error, 0, len(checkpointPins)+len(artifactPins)+8)
	for _, checkpoint := range checkpointPins {
		closeErrors = append(closeErrors, closeEntryReference(checkpoint.entry.pin))
	}
	for _, artifact := range artifactPins {
		closeErrors = append(closeErrors, closeEntryReference(artifact.entry.pin))
	}
	closeErrors = append(closeErrors,
		closeResumeOwnedDirectory(records), closeResumeOwnedDirectory(anchors),
		closeResumeOwnedDirectory(stages), closeDirectory(intent), closeEntryReference(intentPin),
	)
	if root != nil {
		closeErrors = append(closeErrors, root.Close())
	}
	// The runtime lock is released only after every capability that could observe
	// or mutate the selected namespace has closed.
	closeErrors = append(closeErrors, lease.Close())
	closeErr := projectResumeError("close leased resume repository", errors.Join(closeErrors...))
	repository.closeErr = closeErr
	repository.mu.Unlock()
	return closeErr
}

func closeResumeOwnedDirectory(directory *resumeOwnedDirectory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

var _ LeasedRepository = (*resumeLeasedRepository)(nil)
