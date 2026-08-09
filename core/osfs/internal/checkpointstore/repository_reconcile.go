package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

type AttentionCode string

const (
	AttentionUnknownShard         AttentionCode = "unknown-shard"
	AttentionUnknownEntry         AttentionCode = "unknown-entry"
	AttentionCorruptRecord        AttentionCode = "corrupt-record"
	AttentionInvalidBinding       AttentionCode = "invalid-binding"
	AttentionUncommittedRecord    AttentionCode = "uncommitted-record"
	AttentionInvalidCandidate     AttentionCode = "invalid-candidate"
	AttentionOrphanedCandidate    AttentionCode = "orphaned-candidate"
	AttentionConflictingCandidate AttentionCode = "conflicting-candidate"
)

func (code AttentionCode) Valid() bool {
	switch code {
	case AttentionUnknownShard, AttentionUnknownEntry, AttentionCorruptRecord,
		AttentionInvalidBinding, AttentionUncommittedRecord, AttentionInvalidCandidate,
		AttentionOrphanedCandidate, AttentionConflictingCandidate:
		return true
	default:
		return false
	}
}

type Attention struct {
	code      AttentionCode
	reference string
}

func (attention Attention) Code() AttentionCode { return attention.code }

// Reference is a stable diagnostic correlation token, not an internal name or
// user-visible path. Unknown directory contents must not become a logging leak.
func (attention Attention) Reference() string { return attention.reference }

type Snapshot struct {
	records   []checkpointmodel.Record
	attention []Attention
}

func (snapshot Snapshot) Records() []checkpointmodel.Record {
	return slices.Clone(snapshot.records)
}

func (snapshot Snapshot) Attention() []Attention {
	return slices.Clone(snapshot.attention)
}

// CandidateDurabilityWitness proves that a candidate record's owned stage and
// anchor still name the durable file cut that preceded candidate publication.
// The repository owns promotion; the file engine supplies only the platform
// observation it is uniquely able to make.
type CandidateDurabilityWitness func(checkpointmodel.Record) (bool, error)

func (repository *Repository) Reconcile(witness CandidateDurabilityWitness) (Snapshot, error) {
	budget := newRepositoryScanBudget(
		checkpointmodel.MaxCheckpointRecordsPerOperation,
		checkpointmodel.MaxCheckpointAuxiliaryEntriesPerOperation,
	)
	return repository.reconcile(witness, &budget)
}

func (repository *Repository) reconcile(
	witness CandidateDurabilityWitness,
	budget *repositoryScanBudget,
) (Snapshot, error) {
	if repository == nil || repository.records == nil || witness == nil || !budget.valid() {
		return Snapshot{}, transfer.ErrInvalidOutputBinding
	}
	shards, err := repository.records.Names(ShardLimit)
	if err != nil {
		return Snapshot{}, repositoryError("list checkpoint shards", err)
	}
	if len(shards) >= ShardLimit {
		return Snapshot{}, codedError(ErrorCorruptRecord, "bound checkpoint shards", checkpointmodel.ErrRecordRecovery)
	}
	slices.Sort(shards)
	result := Snapshot{}
	for _, shardName := range shards {
		if !ValidShard(shardName) {
			result.attention = append(result.attention, newAttention(AttentionUnknownShard, shardName, ""))
			continue
		}
		kind, exact, err := repository.records.ClassifyExactEntry(shardName)
		if err != nil {
			return Snapshot{}, repositoryError("classify checkpoint shard", err)
		}
		if !exact || kind != outputcap.EntryDirectory {
			result.attention = append(result.attention, newAttention(AttentionUnknownShard, shardName, ""))
			continue
		}
		if err := repository.reconcileShard(shardName, witness, budget, &result); err != nil {
			return Snapshot{}, err
		}
	}
	return result, nil
}

type storedRecord struct {
	record  checkpointmodel.Record
	encoded []byte
}

type repositoryScanBudget struct {
	recordLimit    int
	auxiliaryLimit int
	records        int
	auxiliary      int
}

func newRepositoryScanBudget(recordLimit, auxiliaryLimit int) repositoryScanBudget {
	return repositoryScanBudget{recordLimit: recordLimit, auxiliaryLimit: auxiliaryLimit}
}

func (budget *repositoryScanBudget) valid() bool {
	return budget != nil && budget.recordLimit > 0 && budget.auxiliaryLimit > 0 &&
		budget.records >= 0 && budget.records <= budget.recordLimit &&
		budget.auxiliary >= 0 && budget.auxiliary <= budget.auxiliaryLimit
}

func (budget *repositoryScanBudget) namesLimit() (int, error) {
	if !budget.valid() {
		return 0, checkpointmodel.ErrRecordRecovery
	}
	remaining := budget.recordLimit - budget.records + budget.auxiliaryLimit - budget.auxiliary
	return remaining + 1, nil
}

// observe counts names before their contents grant any authority. Malformed and
// opaque names consume the auxiliary half, so retaining suspicious state cannot
// amplify restart work beyond the same bound reserved for crash candidates.
func (budget *repositoryScanBudget) observe(shard, name string) error {
	if !budget.valid() {
		return checkpointmodel.ErrRecordRecovery
	}
	if !IsTemporaryName(name) {
		if _, err := parseRecordLocation(shard, name); err == nil {
			if budget.records >= budget.recordLimit {
				return checkpointmodel.ErrRecordRecovery
			}
			budget.records++
			return nil
		}
	}
	if budget.auxiliary >= budget.auxiliaryLimit {
		return checkpointmodel.ErrRecordRecovery
	}
	budget.auxiliary++
	return nil
}

func (repository *Repository) reconcileShard(
	shardName string,
	witness CandidateDurabilityWitness,
	budget *repositoryScanBudget,
	result *Snapshot,
) (resultErr error) {
	shard, err := OpenShard(repository.records, shardName, false)
	if err != nil {
		return repositoryError("open checkpoint shard", err)
	}
	defer func() {
		resultErr = transferfault.ReduceBoundaryErrorSet(
			resultErr, repositoryError("close checkpoint shard", shard.Close()),
		)
	}()
	names, err := scanRepositoryShardNames(shard, shardName, budget)
	if err != nil {
		return err
	}
	stable, occupied, err := repository.loadStableShardRecords(shard, shardName, names, result)
	if err != nil {
		return err
	}
	if err := repository.reconcileShardCandidates(
		shard, shardName, names, stable, occupied, result,
	); err != nil {
		return err
	}
	return repository.appendReconciledShardRecords(shard, shardName, stable, witness, result)
}

func scanRepositoryShardNames(
	shard outputcap.Directory,
	shardName string,
	budget *repositoryScanBudget,
) ([]string, error) {
	entryLimit, err := budget.namesLimit()
	if err != nil {
		return nil, codedError(ErrorCorruptRecord, "bound checkpoint entries", err)
	}
	names, err := shard.Names(entryLimit)
	if err != nil {
		return nil, repositoryError("list checkpoint entries", err)
	}
	if len(names) >= entryLimit {
		return nil, codedError(ErrorCorruptRecord, "bound checkpoint entries", checkpointmodel.ErrRecordRecovery)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := budget.observe(shardName, name); err != nil {
			return nil, codedError(ErrorCorruptRecord, "bound checkpoint entries", err)
		}
	}
	return names, nil
}

func (repository *Repository) loadStableShardRecords(
	shard outputcap.Directory,
	shardName string,
	names []string,
	result *Snapshot,
) (map[string]storedRecord, map[string]struct{}, error) {
	stable := make(map[string]storedRecord)
	occupied := make(map[string]struct{})
	for _, name := range names {
		if IsTemporaryName(name) {
			continue
		}
		occupied[name] = struct{}{}
		recordID, parseErr := parseRecordLocation(shardName, name)
		if parseErr != nil {
			result.attention = append(result.attention, newAttention(AttentionUnknownEntry, shardName, name))
			continue
		}
		loaded, attention, loadErr := repository.loadRecordImage(shard, shardName, name, recordID)
		if loadErr != nil {
			return nil, nil, loadErr
		}
		if attention != nil {
			result.attention = append(result.attention, *attention)
			continue
		}
		stable[name] = loaded
	}
	return stable, occupied, nil
}

func (repository *Repository) reconcileShardCandidates(
	shard outputcap.Directory,
	shardName string,
	names []string,
	stable map[string]storedRecord,
	occupied map[string]struct{},
	result *Snapshot,
) error {
	for _, name := range names {
		if !IsTemporaryName(name) {
			continue
		}
		if err := repository.reconcileCandidate(shard, shardName, name, stable, occupied, result); err != nil {
			return err
		}
	}
	return nil
}

func (repository *Repository) appendReconciledShardRecords(
	shard outputcap.Directory,
	shardName string,
	stable map[string]storedRecord,
	witness CandidateDurabilityWitness,
	result *Snapshot,
) error {
	stableNames := make([]string, 0, len(stable))
	for name := range stable {
		stableNames = append(stableNames, name)
	}
	slices.Sort(stableNames)
	for _, name := range stableNames {
		loaded, err := reconcileCandidateRecord(shard, name, stable[name], witness)
		if err != nil {
			return err
		}
		stable[name] = loaded
		if !checkpointmodel.Committed(loaded.record) {
			result.attention = append(result.attention, newAttention(AttentionUncommittedRecord, shardName, name))
			continue
		}
		result.records = append(result.records, loaded.record)
	}
	return nil
}

func reconcileCandidateRecord(
	shard outputcap.Directory,
	name string,
	loaded storedRecord,
	witness CandidateDurabilityWitness,
) (storedRecord, error) {
	if loaded.record.CommitState() != checkpointmodel.CommitCandidate {
		return loaded, nil
	}
	recoverable, err := witness(loaded.record)
	if err != nil {
		return storedRecord{}, repositoryError("verify checkpoint candidate witness", err)
	}
	if !recoverable {
		return loaded, nil
	}
	promoted, err := checkpointmodel.Promote(
		loaded.record, loaded.record.Phase(), checkpointmodel.CommitVerified,
	)
	if err != nil {
		return storedRecord{}, codedError(ErrorCorruptRecord, "promote checkpoint candidate", err)
	}
	promotedEncoded, err := checkpointmodel.EncodeRecord(promoted)
	if err != nil {
		return storedRecord{}, codedError(ErrorCorruptRecord, "encode promoted checkpoint", err)
	}
	if err := InstallReplace(shard, name, loaded.encoded, promotedEncoded); err != nil {
		return storedRecord{}, repositoryError("install promoted checkpoint", err)
	}
	return storedRecord{record: promoted, encoded: promotedEncoded}, nil
}

func (repository *Repository) loadRecordImage(
	shard outputcap.Directory,
	shardName string,
	name string,
	recordID checkpointmodel.RecordID,
) (storedRecord, *Attention, error) {
	kind, exact, err := shard.ClassifyExactEntry(name)
	if err != nil {
		return storedRecord{}, nil, repositoryError("classify checkpoint record", err)
	}
	if !exact || kind != outputcap.EntryRegularFile {
		attention := newAttention(AttentionCorruptRecord, shardName, name)
		return storedRecord{}, &attention, nil
	}
	encoded, err := ReadFile(shard, name)
	if err != nil {
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			attention := newAttention(AttentionCorruptRecord, shardName, name)
			return storedRecord{}, &attention, nil
		}
		return storedRecord{}, nil, repositoryError("read checkpoint record", err)
	}
	record, err := checkpointmodel.DecodeRecord(encoded)
	if err != nil {
		attention := newAttention(AttentionCorruptRecord, shardName, name)
		return storedRecord{}, &attention, nil
	}
	if !repository.binding.Matches(record, recordID) {
		attention := newAttention(AttentionInvalidBinding, shardName, name)
		return storedRecord{}, &attention, nil
	}
	return storedRecord{record: record, encoded: encoded}, nil, nil
}

func (repository *Repository) reconcileCandidate(
	shard outputcap.Directory,
	shardName string,
	name string,
	stable map[string]storedRecord,
	occupied map[string]struct{},
	result *Snapshot,
) error {
	kind, exact, err := shard.ClassifyExactEntry(name)
	if err != nil {
		return repositoryError("classify checkpoint candidate", err)
	}
	if kind == outputcap.EntryAbsent {
		// An earlier candidate from the same bounded snapshot may have reconciled
		// every exact duplicate. Absence already satisfies that cleanup decision.
		return nil
	}
	if !exact || kind != outputcap.EntryRegularFile {
		result.attention = append(result.attention, newAttention(AttentionInvalidCandidate, shardName, name))
		return nil
	}
	encoded, err := ReadFile(shard, name)
	if err != nil {
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			result.attention = append(result.attention, newAttention(AttentionInvalidCandidate, shardName, name))
			return nil
		}
		return repositoryError("read checkpoint candidate", err)
	}
	candidate, err := checkpointmodel.DecodeRecord(encoded)
	if err != nil {
		result.attention = append(result.attention, newAttention(AttentionInvalidCandidate, shardName, name))
		return nil
	}
	targetShard, targetName := recordLocation(candidate.RecordID())
	if targetShard != shardName || !MatchesTemporaryName(name, targetName, encoded) ||
		!repository.binding.Matches(candidate, candidate.RecordID()) {
		result.attention = append(result.attention, newAttention(AttentionInvalidCandidate, shardName, name))
		return nil
	}
	current, found := stable[targetName]
	if !found {
		if _, targetOccupied := occupied[targetName]; targetOccupied {
			result.attention = append(result.attention, newAttention(AttentionConflictingCandidate, shardName, name))
			return nil
		}
		if !checkpointmodel.InitialCandidate(candidate) {
			result.attention = append(result.attention, newAttention(AttentionOrphanedCandidate, shardName, name))
			return nil
		}
		if err := InstallCreate(shard, targetName, encoded); err != nil {
			return repositoryError("install initial checkpoint candidate", err)
		}
		stable[targetName] = storedRecord{record: candidate, encoded: encoded}
		occupied[targetName] = struct{}{}
		return nil
	}
	if bytes.Equal(current.encoded, encoded) {
		if err := InstallCreate(shard, targetName, encoded); err != nil {
			return repositoryError("settle duplicate checkpoint candidate", err)
		}
		return nil
	}
	if candidate.CommitState() == checkpointmodel.CommitCandidate &&
		checkpointmodel.Committed(current.record) &&
		(checkpointmodel.ValidateTransition(candidate, current.record) == nil ||
			checkpointmodel.ValidateTransition(current.record, candidate) == nil) {
		// The fixed target remains the authority until replacement. A valid
		// unlinked write-ahead image may be either the predecessor of an already
		// promoted target or its next candidate; neither can grant have-state by
		// its private installation name.
		if err := RemoveExactTemporary(shard, name, encoded); err != nil {
			return repositoryError("remove superseded checkpoint candidate", err)
		}
		return nil
	}
	if !checkpointmodel.Committed(candidate) || checkpointmodel.ValidateTransition(current.record, candidate) != nil {
		result.attention = append(result.attention, newAttention(AttentionConflictingCandidate, shardName, name))
		return nil
	}
	if err := InstallReplace(shard, targetName, current.encoded, encoded); err != nil {
		return repositoryError("install checkpoint candidate", err)
	}
	stable[targetName] = storedRecord{record: candidate, encoded: encoded}
	return nil
}

func newAttention(code AttentionCode, shard, name string) Attention {
	hash := sha256.New()
	_, _ = hash.Write([]byte(attentionDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(shard))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(name))
	return Attention{code: code, reference: hex.EncodeToString(hash.Sum(nil))}
}

func recordLocation(recordID checkpointmodel.RecordID) (string, string) {
	encoded := hex.EncodeToString(recordID.Bytes())
	return encoded[:recordShardLength], encoded + recordSuffix
}

func RecordLocation(recordID checkpointmodel.RecordID) (string, string) {
	return recordLocation(recordID)
}

func parseRecordLocation(shard, name string) (checkpointmodel.RecordID, error) {
	if !ValidShard(shard) || len(name) != recordIDHexLength+len(recordSuffix) ||
		!strings.HasSuffix(name, recordSuffix) || name[:recordShardLength] != shard {
		return checkpointmodel.RecordID{}, checkpointmodel.ErrRecordBinding
	}
	encoded := strings.TrimSuffix(name, recordSuffix)
	if encoded != strings.ToLower(encoded) {
		return checkpointmodel.RecordID{}, checkpointmodel.ErrRecordBinding
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return checkpointmodel.RecordID{}, checkpointmodel.ErrRecordBinding
	}
	return checkpointmodel.RecordIDFromBytes(raw)
}

func ParseRecordLocation(shard, name string) (checkpointmodel.RecordID, error) {
	return parseRecordLocation(shard, name)
}
