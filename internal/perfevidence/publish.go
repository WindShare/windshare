package perfevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type regularFileVisitor func(relative string, file *os.File, info os.FileInfo) error

const (
	payloadName              = "payload.json"
	manifestName             = "manifest.json"
	stageOwnerName           = "owner.json"
	abandonedStageMinimumAge = 24 * time.Hour
	evidenceFileReadBatch    = 64 << 10
)

// EvidenceStoreBudget is the single resource contract for untrusted evidence
// namespaces. Publication and crash recovery share it so neither path can
// become the other's unbounded parser or filesystem walker.
type EvidenceStoreBudget struct {
	MaxRootEntries   int
	MaxObjects       int
	MaxDepth         int
	MaxMetadataBytes int64
	MaxPayloadBytes  int64
	MaxTotalBytes    int64
}

func DefaultEvidenceStoreBudget() EvidenceStoreBudget {
	return EvidenceStoreBudget{
		MaxRootEntries:   4_096,
		MaxObjects:       16_384,
		MaxDepth:         32,
		MaxMetadataBytes: 1 << 20,
		MaxPayloadBytes:  32 << 20,
		// Seven workload binaries and their CPU/memory profiles can approach
		// 18 GiB under the command-level limits; this leaves deliberate room
		// for logs without permitting an unbounded evidence tree.
		MaxTotalBytes: 32 << 30,
	}
}

type evidenceStoreFileClass uint8

const (
	evidenceArtifactFile evidenceStoreFileClass = iota
	evidenceMetadataFile
	evidencePayloadFile
)

type evidenceStoreMeter struct {
	budget      EvidenceStoreBudget
	rootEntries int
	objects     int
	totalBytes  int64
}
type evidenceStoreWalk struct {
	meter     *evidenceStoreMeter
	visitor   regularFileVisitor
	skipFiles map[string]struct{}
}

func newEvidenceStoreMeter(budget EvidenceStoreBudget) (*evidenceStoreMeter, error) {
	if budget.MaxRootEntries <= 0 || budget.MaxObjects <= 0 || budget.MaxDepth <= 0 ||
		budget.MaxMetadataBytes <= 0 || budget.MaxPayloadBytes <= 0 || budget.MaxTotalBytes <= 0 {
		return nil, errors.New("evidence store budget limits must all be positive")
	}
	if budget.MaxMetadataBytes > budget.MaxTotalBytes || budget.MaxPayloadBytes > budget.MaxTotalBytes {
		return nil, errors.New("evidence per-file limits must not exceed the total-byte limit")
	}
	return &evidenceStoreMeter{budget: budget}, nil
}

func (meter *evidenceStoreMeter) observeRootEntries(count int) error {
	if count < 0 || meter.rootEntries > meter.budget.MaxRootEntries-count {
		return fmt.Errorf("evidence root exceeds %d entries", meter.budget.MaxRootEntries)
	}
	meter.rootEntries += count
	return nil
}

func (meter *evidenceStoreMeter) observeFile(
	relative string,
	depth int,
	bytes int64,
	class evidenceStoreFileClass,
) error {
	return meter.observeObject(relative, depth, bytes, class)
}

func (meter *evidenceStoreMeter) observeObject(
	relative string,
	depth int,
	bytes int64,
	class evidenceStoreFileClass,
) error {
	if meter == nil {
		return errors.New("evidence store meter is nil")
	}
	if depth <= 0 || depth > meter.budget.MaxDepth {
		return fmt.Errorf("evidence object %s exceeds maximum depth %d", relative, meter.budget.MaxDepth)
	}
	if meter.objects >= meter.budget.MaxObjects {
		return fmt.Errorf("evidence tree exceeds %d objects", meter.budget.MaxObjects)
	}
	if bytes < 0 {
		return fmt.Errorf("evidence object %s has a negative size", relative)
	}
	switch class {
	case evidenceMetadataFile:
		if bytes > meter.budget.MaxMetadataBytes {
			return fmt.Errorf("evidence metadata %s exceeds %d bytes", relative, meter.budget.MaxMetadataBytes)
		}
	case evidencePayloadFile:
		if bytes > meter.budget.MaxPayloadBytes {
			return fmt.Errorf("evidence payload %s exceeds %d bytes", relative, meter.budget.MaxPayloadBytes)
		}
	}
	if bytes > meter.budget.MaxTotalBytes-meter.totalBytes {
		return fmt.Errorf("evidence tree exceeds %d total bytes", meter.budget.MaxTotalBytes)
	}
	meter.objects++
	meter.totalBytes += bytes
	return nil
}

var ErrAlreadyPublished = errors.New("performance evidence already exists")

type publicationManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	EvidenceID    string `json:"evidenceId"`
	PayloadFile   string `json:"payloadFile"`
	PayloadSHA256 string `json:"payloadSha256"`
}
type stageOwner struct {
	SchemaVersion int       `json:"schemaVersion"`
	RunID         string    `json:"runId"`
	ProcessID     int       `json:"processId"`
	ProcessToken  string    `json:"processToken"`
	CreatedAt     time.Time `json:"createdAt"`
}

// Stage owns both publishable artifacts and non-publishable build caches. The
// caller must defer Abort immediately; Commit disarms artifact cleanup only
// after a verified destination exists.
type Stage struct {
	OutputRoot   string
	ArtifactRoot string
	RuntimeRoot  string
	authority    *outputRootAuthority
	artifactDir  *stageDirectoryAuthority
	runtimeDir   *stageDirectoryAuthority
	artifactName string
	runtimeName  string
	runID        string
	transition   func(string) error
	storeBudget  EvidenceStoreBudget
	mu           sync.Mutex
	committed    bool
}

func NewStage(outputRoot, runID string) (*Stage, error) {
	return newStage(outputRoot, runID, time.Now().UTC(), nil)
}

func newStage(outputRoot, runID string, now time.Time, transition func(string) error) (*Stage, error) {
	authority, err := openOutputRootAuthority(outputRoot)
	if err != nil {
		return nil, err
	}
	return newStageWithAuthority(authority, runID, now, transition)
}

func newStageWithAuthority(
	authority *outputRootAuthority,
	runID string,
	now time.Time,
	transition func(string) error,
) (*Stage, error) {
	if authority == nil {
		return nil, errors.New("evidence output authority is nil")
	}
	if !validRunID(runID) {
		return nil, errors.Join(
			fmt.Errorf("performance run ID %q is not path-safe", runID), authority.close(),
		)
	}
	if err := authority.verifyPath(); err != nil {
		return nil, errors.Join(err, authority.close())
	}
	if err := recoverAbandonedStagesWithAuthority(authority, now); err != nil {
		return nil, errors.Join(err, authority.close())
	}
	root := authority.path
	artifactName := ".staging-" + runID
	runtimeName := ".runtime-" + runID
	stage := &Stage{
		OutputRoot:   root,
		authority:    authority,
		artifactName: artifactName,
		runtimeName:  runtimeName,
		runID:        runID,
		transition:   transition,
		storeBudget:  DefaultEvidenceStoreBudget(),
	}
	runtimeDir, err := authority.createChildAuthority(runtimeName)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create evidence runtime directory: %w", err), stage.Abort())
	}
	stage.runtimeDir = runtimeDir
	stage.RuntimeRoot = runtimeDir.path
	if err := runtimeDir.acquireLiveLease(authority); err != nil {
		return nil, errors.Join(fmt.Errorf("acquire runtime live-stage lease: %w", err), stage.Abort())
	}
	processToken, err := currentProcessToken()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("identify stage owner process: %w", err), stage.Abort())
	}
	owner, err := json.Marshal(stageOwner{
		SchemaVersion: SchemaVersion, RunID: runID, ProcessID: os.Getpid(),
		ProcessToken: processToken, CreatedAt: now,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("encode stage owner: %w", err), stage.Abort())
	}
	if err := writeExclusive(filepath.Join(stage.RuntimeRoot, stageOwnerName), owner); err != nil {
		return nil, errors.Join(fmt.Errorf("write stage owner: %w", err), stage.Abort())
	}
	if err := stage.runtimeDir.syncContents(); err != nil {
		return nil, errors.Join(fmt.Errorf("sync stage owner: %w", err), stage.Abort())
	}
	artifactDir, err := authority.createChildAuthority(artifactName)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create evidence staging directory: %w", err), stage.Abort())
	}
	stage.artifactDir = artifactDir
	stage.ArtifactRoot = artifactDir.path
	if err := artifactDir.acquireLiveLease(authority); err != nil {
		return nil, errors.Join(fmt.Errorf("acquire artifact live-stage lease: %w", err), stage.Abort())
	}
	return stage, nil
}

func (stage *Stage) Abort() error {
	if stage == nil {
		return nil
	}
	stage.mu.Lock()
	defer stage.mu.Unlock()
	if stage.authority == nil {
		return nil
	}
	var errs []error
	if stage.artifactDir != nil {
		errs = append(errs, stage.artifactDir.close())
		stage.artifactDir = nil
	}
	if stage.runtimeDir != nil {
		errs = append(errs, stage.runtimeDir.close())
		stage.runtimeDir = nil
	}
	if !stage.committed {
		if err := removeOwnedTreeAuthority(stage.authority, stage.artifactName, nil); err != nil {
			errs = append(errs, fmt.Errorf("remove abandoned evidence stage: %w", err))
		}
	}
	if err := removeOwnedTreeAuthority(stage.authority, stage.runtimeName, nil); err != nil {
		errs = append(errs, fmt.Errorf("remove performance runtime: %w", err))
	}
	if stage.authority != nil {
		errs = append(errs, stage.authority.close())
		stage.authority = nil
	}
	return errors.Join(errs...)
}

func (stage *Stage) Commit(
	evidence Evidence,
	validate func() error,
	release func() error,
) (Publication, error) {
	if stage == nil {
		return Publication{}, errors.New("performance evidence stage is nil")
	}
	stage.mu.Lock()
	defer stage.mu.Unlock()
	if stage.committed {
		return Publication{}, errors.New("performance evidence stage was already committed")
	}
	publication, err := publishArtifactRoot(
		stage.authority, stage.artifactDir, evidence, validate, stage.transition, stage.storeBudget,
	)
	if err != nil {
		return Publication{}, err
	}
	stage.committed = true
	var cleanupErrs []error
	if release != nil {
		cleanupErrs = append(cleanupErrs, release())
	}
	cleanupErrs = append(cleanupErrs, stage.artifactDir.close())
	stage.artifactDir = nil
	cleanupErrs = append(cleanupErrs, stage.runtimeDir.close())
	stage.runtimeDir = nil
	if err := removeOwnedTreeAuthority(stage.authority, stage.runtimeName, nil); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("publication succeeded but runtime cleanup failed: %w", err))
	}
	cleanupErrs = append(cleanupErrs, stage.authority.close())
	stage.authority = nil
	if err := errors.Join(cleanupErrs...); err != nil {
		return publication, fmt.Errorf("publication cleanup: %w", err)
	}
	return publication, nil
}

func publishArtifactRoot(
	authority *outputRootAuthority,
	artifactDir *stageDirectoryAuthority,
	evidence Evidence,
	validate func() error,
	transition func(string) error,
	budget EvidenceStoreBudget,
) (publication Publication, resultErr error) {
	if _, err := newEvidenceStoreMeter(budget); err != nil {
		return Publication{}, err
	}
	root, stagePath, err := publicationPathsWithAuthority(authority, artifactDir)
	if err != nil {
		return Publication{}, err
	}
	if err := runPublicationTransition(transition, "before-artifact-inspection"); err != nil {
		return Publication{}, err
	}
	if err := runPublicationValidation(validate, "before-artifact-inspection"); err != nil {
		return Publication{}, err
	}
	artifacts, err := inspectArtifactsAuthorityWithBudget(artifactDir, budget)
	if err != nil {
		return Publication{}, err
	}
	evidence.Artifacts = artifacts
	if err := runPublicationValidation(validate, "after-artifact-inspection"); err != nil {
		return Publication{}, err
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		return Publication{}, fmt.Errorf("encode performance evidence: %w", err)
	}
	if err := validateEvidenceDocumentSize(payloadName, int64(len(payload)), evidencePayloadFile, budget); err != nil {
		return Publication{}, err
	}
	evidenceID := hashBytes(payload)
	if err := writeExclusive(filepath.Join(stagePath, payloadName), payload); err != nil {
		return Publication{}, fmt.Errorf("write evidence payload: %w", err)
	}
	manifest, err := json.Marshal(publicationManifest{
		SchemaVersion: SchemaVersion, EvidenceID: evidenceID,
		PayloadFile: payloadName, PayloadSHA256: evidenceID,
	})
	if err != nil {
		return Publication{}, fmt.Errorf("encode evidence manifest: %w", err)
	}
	if err := validateEvidenceDocumentSize(manifestName, int64(len(manifest)), evidenceMetadataFile, budget); err != nil {
		return Publication{}, err
	}
	if err := writeExclusive(filepath.Join(stagePath, manifestName), manifest); err != nil {
		return Publication{}, fmt.Errorf("write evidence manifest: %w", err)
	}
	if err := verifyPublicationAuthorityWithBudget(artifactDir, evidenceID, budget); err != nil {
		return Publication{}, fmt.Errorf("verify staged evidence: %w", err)
	}
	if err := artifactDir.syncContents(); err != nil {
		return Publication{}, fmt.Errorf("sync staged evidence: %w", err)
	}
	destination := filepath.Join(root, evidenceID)
	if err := runPublicationTransition(transition, "before-rename"); err != nil {
		return Publication{}, err
	}
	if err := verifyPublicationAuthorityWithBudget(artifactDir, evidenceID, budget); err != nil {
		return Publication{}, fmt.Errorf("verify staged evidence at rename boundary: %w", err)
	}
	if err := runPublicationTransition(transition, "before-final-source-validation"); err != nil {
		return Publication{}, err
	}
	if err := authority.verifyPath(); err != nil {
		return Publication{}, fmt.Errorf("verify output authority at rename boundary: %w", err)
	}
	if err := artifactDir.verifyName(authority); err != nil {
		return Publication{}, fmt.Errorf("verify stage authority at rename boundary: %w", err)
	}
	if err := runPublicationTransition(transition, "after-rename-boundary-verification"); err != nil {
		return Publication{}, err
	}
	// Transition hooks are deliberately kept before the final verification and
	// seal. Once the seal exists, no user callback can create a gap between the
	// proven namespace/bytes and the no-replace rename.
	if err := verifyPublicationAuthorityWithBudget(artifactDir, evidenceID, budget); err != nil {
		return Publication{}, fmt.Errorf("verify staged evidence after rename-boundary transition: %w", err)
	}
	if err := runPublicationValidation(validate, "immediately-before-rename"); err != nil {
		return Publication{}, err
	}
	rollbackID, err := randomRunID()
	if err != nil {
		return Publication{}, fmt.Errorf("allocate failed-publication quarantine identity: %w", err)
	}
	rollbackName := ".staging-rollback-" + rollbackID
	seal, err := acquirePublicationSeal(artifactDir, budget)
	if err != nil {
		return Publication{}, fmt.Errorf("seal staged evidence for publication: %w", err)
	}
	renamed := false
	var publishedDir, rollbackDir *stageDirectoryAuthority
	defer func() {
		if seal != nil {
			resultErr = errors.Join(resultErr, seal.Close())
			seal = nil
		}
		if resultErr != nil && renamed {
			resultErr = errors.Join(
				resultErr,
				rollbackFailedPublication(authority, rollbackDir, evidenceID, rollbackName, budget),
			)
		}
		if publishedDir != nil {
			resultErr = errors.Join(resultErr, publishedDir.close())
		}
	}()
	if err := preparePublicationRename(seal, stagePath); err != nil {
		return Publication{}, fmt.Errorf("prepare sealed evidence rename: %w", err)
	}
	if err := authority.renameChildNoReplace(artifactDir, evidenceID); err != nil {
		if closeErr := seal.Close(); closeErr != nil {
			return Publication{}, fmt.Errorf("release failed publication seal: %w", errors.Join(err, closeErr))
		}
		seal = nil
		destinationDir, destinationErr := authority.openChildAuthority(evidenceID)
		if destinationErr == nil {
			return reconcileExistingPublication(
				authority, artifactDir, destinationDir, destination, evidenceID, budget, transition,
			)
		}
		return Publication{}, fmt.Errorf(
			"atomically publish performance evidence: %w",
			errors.Join(err, fmt.Errorf("inspect existing destination: %w", destinationErr)),
		)
	}
	renamed = true
	artifactDir.name = evidenceID
	rollbackDir = artifactDir
	publishedDir, err = authority.openRecoveryChildAuthority(evidenceID)
	if err != nil {
		return Publication{}, fmt.Errorf("retain published evidence authority: %w", err)
	}
	matchErr := artifactDir.matchesAuthority(publishedDir)
	leased, err := publishedDir.tryAcquireRecoveryLease(authority)
	if err != nil {
		rollbackDir = publishedDir
		return Publication{}, fmt.Errorf("retain published evidence rollback lease: %w", err)
	}
	if matchErr != nil || leased {
		rollbackDir = publishedDir
	} else if err := artifactDir.verifyName(authority); err != nil {
		return Publication{}, fmt.Errorf("retain original publication lease: %w", err)
	}
	if err := completePublicationRename(seal, destination); err != nil {
		return Publication{}, fmt.Errorf("complete sealed evidence rename: %w", err)
	}
	if matchErr != nil {
		return Publication{}, fmt.Errorf("publication renamed a substituted stage object: %w", matchErr)
	}
	if err := runPublicationTransition(transition, "after-rename"); err != nil {
		return Publication{}, err
	}
	if err := verifyPublicationAuthorityWithBudget(publishedDir, evidenceID, budget); err != nil {
		return Publication{}, fmt.Errorf("verify published evidence: %w", err)
	}
	if err := authority.sync(); err != nil {
		return Publication{}, fmt.Errorf("sync evidence publication root: %w", err)
	}
	if err := runPublicationTransition(transition, "after-verification"); err != nil {
		return Publication{}, err
	}
	// The returned pathname is trustworthy only while it still names the two
	// retained objects used for the rename and the final byte verification.
	// Rechecking both identities also rejects a destination-name swap after the
	// durability boundary without ever following the replacement.
	if err := authority.verifyPath(); err != nil {
		return Publication{}, fmt.Errorf("verify output authority after publication: %w", err)
	}
	if err := publishedDir.verifyName(authority); err != nil {
		return Publication{}, fmt.Errorf("verify publication name after publication: %w", err)
	}
	if err := seal.Verify(); err != nil {
		return Publication{}, fmt.Errorf("verify sealed publication: %w", err)
	}
	if err := seal.Close(); err != nil {
		seal = nil
		return Publication{}, fmt.Errorf("release sealed publication: %w", err)
	}
	seal = nil
	return Publication{EvidenceID: evidenceID, Path: destination}, nil
}

type publicationRenamePreparer interface {
	preparePublicationRename(source string) error
}
type publicationRenameCompleter interface {
	completePublicationRename(destination string) error
}

func preparePublicationRename(seal byteConsumptionAuthority, source string) error {
	if preparer, ok := seal.(publicationRenamePreparer); ok {
		return preparer.preparePublicationRename(source)
	}
	return seal.Verify()
}

func completePublicationRename(seal byteConsumptionAuthority, destination string) error {
	if completer, ok := seal.(publicationRenameCompleter); ok {
		return completer.completePublicationRename(destination)
	}
	return seal.Verify()
}

func verifyBoundArtifact(
	identity ArtifactFile,
	artifacts map[string]ArtifactFile,
	label string,
	requireNonempty bool,
) error {
	actual, found := artifacts[identity.Path]
	if !found || identity.Path == "" || identity.Bytes < 0 || !validSHA256(identity.SHA256) || actual != identity {
		return fmt.Errorf("%s is not bound to its exact manifest artifact", label)
	}
	if requireNonempty && identity.Bytes <= 0 {
		return fmt.Errorf("%s must be non-empty", label)
	}
	return nil
}

func commandClaimsArtifact(command CommandEvidence, identity ArtifactFile) bool {
	return slices.Contains(command.Artifacts, identity)
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
