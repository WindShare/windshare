package checkpointstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const (
	temporaryCandidatePrefix = ".candidate-"
	candidateImageHashBytes  = 8
	sharedCandidates         = candidateNamespace(temporaryCandidatePrefix)
)

// A held operation lease owns its reserved temporary slots independently of
// their bytes. Shared bootstrap writes have no such owner and must continue to
// prove an exact image before adopting or removing an existing candidate.
type candidateNamespace string

func operationCandidates(operation receivecontract.OperationID) candidateNamespace {
	if operation.IsZero() {
		return ""
	}
	return candidateNamespace(temporaryCandidatePrefix + "operation-" + hex.EncodeToString(operation.Bytes()) + "-")
}

func (namespace candidateNamespace) name(target string, encoded []byte, attempt int) string {
	hash := sha256.Sum256(append([]byte(target+"\x00"), encoded...))
	return string(namespace) + hex.EncodeToString(hash[:candidateImageHashBytes]) + "-" + strconv.Itoa(attempt)
}

func (namespace candidateNamespace) matches(candidate, target string, encoded []byte) bool {
	for attempt := range installationAttempts {
		if candidate == namespace.name(target, encoded, attempt) {
			return true
		}
	}
	return false
}

func (namespace candidateNamespace) owns(candidate string) bool {
	if namespace == "" || namespace == sharedCandidates {
		return false
	}
	suffix, found := strings.CutPrefix(candidate, string(namespace))
	if !found {
		return false
	}
	digest, slot, found := strings.Cut(suffix, "-")
	if !found || !validLowerHexName(digest, candidateImageHashBytes) {
		return false
	}
	attempt, err := strconv.Atoi(slot)
	return err == nil && attempt >= 0 && attempt < installationAttempts && strconv.Itoa(attempt) == slot
}

func TemporaryName(name string, encoded []byte, attempt int) string {
	return sharedCandidates.name(name, encoded, attempt)
}

// OperationTemporaryName reserves a scratch slot within an authenticated,
// exclusively leased operation. It grants no authority to reuse the payload.
func OperationTemporaryName(operation receivecontract.OperationID, name string, encoded []byte, attempt int) string {
	return operationCandidates(operation).name(name, encoded, attempt)
}

func IsTemporaryName(name string) bool {
	return len(name) > len(temporaryCandidatePrefix) && strings.HasPrefix(name, temporaryCandidatePrefix)
}

func MatchesTemporaryName(candidate, target string, encoded []byte) bool {
	return sharedCandidates.matches(candidate, target, encoded)
}

type recordInstaller struct {
	directory  outputcap.Directory
	candidates candidateNamespace
}

// A physical image can prove cleanup ownership without proving resumable
// progress. Keep its exact name so retiring the fixed record cannot orphan the
// independent evidence carried by an authenticated temporary image.
type checkpointImage struct {
	record checkpointmodel.Record
	name   string
}

func fixedCheckpointImage(record checkpointmodel.Record) checkpointImage {
	_, name := recordLocation(record.RecordID())
	return checkpointImage{record: record, name: name}
}

func (image checkpointImage) reference() string {
	shard, _ := recordLocation(image.record.RecordID())
	return attentionReference(shard, image.name)
}

func (snapshot *Snapshot) retainCandidate(record checkpointmodel.Record, name string, code AttentionCode) {
	image := checkpointImage{record: record, name: name}
	snapshot.candidates = append(snapshot.candidates, image)
	snapshot.attention = append(snapshot.attention, Attention{code: code, reference: image.reference()})
}

func (repository *Repository) installer(directory outputcap.Directory) recordInstaller {
	return recordInstaller{directory: directory, candidates: operationCandidates(repository.binding.OperationID())}
}

// The observed handle pins the object through removal. An empty or undecodable
// image cannot establish recovery authority, but cannot revoke its slot's
// independently established cleanup ownership either.
func (repository *Repository) loadCandidateImage(
	shard outputcap.Directory,
	shardName, name string,
	result *Snapshot,
) (loaded storedRecord, resultErr error) {
	if repository.lease == nil || repository.lease.lock == nil || repository.lease.deleted ||
		repository.lease.Record().OperationID() != repository.binding.OperationID() {
		return storedRecord{}, transfer.ErrInvalidOutputBinding
	}
	file, err := shard.OpenObservedFile(name, true)
	if err == nil && file == nil {
		err = outputcap.ErrUnsafeNamespace
	}
	if err != nil {
		closeErr := closeFile(file)
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			result.attention = append(result.attention, newAttention(AttentionInvalidCandidate, shardName, name))
			return storedRecord{}, repositoryError("close unsafe checkpoint candidate", closeErr)
		}
		return storedRecord{}, repositoryError("open checkpoint candidate", errors.Join(err, closeErr))
	}
	defer func() {
		resultErr = errors.Join(resultErr, repositoryError("close checkpoint candidate", closeFile(file)))
	}()
	size, err := file.Size()
	if err != nil {
		return storedRecord{}, repositoryError("size checkpoint candidate", err)
	}
	if size == 0 || size > maxRepositoryRecordBytes {
		return storedRecord{}, reclaimCandidate(shard, name, file, result)
	}
	encoded, err := readBoundedFile(file)
	if err != nil {
		return storedRecord{}, repositoryError("read checkpoint candidate", err)
	}
	candidate, err := checkpointmodel.DecodeRecord(encoded)
	if err != nil {
		return storedRecord{}, reclaimCandidate(shard, name, file, result)
	}
	return storedRecord{record: candidate, encoded: encoded}, nil
}

func reclaimCandidate(shard outputcap.Directory, name string, file outputcap.ObservedFile, result *Snapshot) error {
	if err := shard.RemoveFile(name, file); err != nil {
		return repositoryError("remove interrupted checkpoint candidate", err)
	}
	if err := shard.Sync(); err != nil {
		return repositoryError("sync reclaimed checkpoint candidate", err)
	}
	result.reclaimedCandidates++
	return nil
}
