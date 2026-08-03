package perfevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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

	mu        sync.Mutex
	committed bool
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

func validateEvidenceDocumentSize(
	name string,
	bytes int64,
	class evidenceStoreFileClass,
	budget EvidenceStoreBudget,
) error {
	meter, err := newEvidenceStoreMeter(budget)
	if err != nil {
		return err
	}
	return meter.observeFile(name, 1, bytes, class)
}

func acquirePublicationSeal(
	authority *stageDirectoryAuthority,
	budget EvidenceStoreBudget,
) (byteConsumptionAuthority, error) {
	meter, err := newEvidenceStoreMeter(budget)
	if err != nil {
		return nil, err
	}
	var targets []snapshotValidationTarget
	err = authority.walkEvidenceStore(&evidenceStoreWalk{meter: meter, visitor: func(
		relative string, file *os.File, info os.FileInfo,
	) error {
		identity, err := artifactIdentityFromOpenFile(file, info, relative)
		if err != nil {
			return err
		}
		targets = append(targets, snapshotValidationTarget{
			LogicalPath:  identity.Path,
			PhysicalPath: filepath.Join(authority.path, filepath.FromSlash(identity.Path)),
			Bytes:        identity.Bytes,
			SHA256:       identity.SHA256,
		})
		return nil
	}})
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("publication seal requires at least one staged file")
	}
	return acquireConsumptionAuthority(targets, []string{authority.path})
}

func rollbackFailedPublication(
	authority *outputRootAuthority,
	retained *stageDirectoryAuthority,
	evidenceID string,
	quarantineName string,
	_ EvidenceStoreBudget,
) error {
	exact, err := retainExactNamedObject(authority, retained, evidenceID)
	if err != nil {
		return fmt.Errorf("retain failed publication for rollback: %w", err)
	}
	if err := authority.renameChildNoReplace(exact, quarantineName); err != nil {
		removeErr := removeExactObjectAtName(authority, exact, evidenceID)
		return fmt.Errorf(
			"quarantine failed publication: %w",
			errors.Join(err, removeErr),
		)
	}
	if err := exact.close(); err != nil {
		return fmt.Errorf("release quarantined publication authority: %w", err)
	}
	if err := removeExactObjectAtName(authority, exact, quarantineName); err != nil {
		return fmt.Errorf("remove quarantined publication: %w", err)
	}
	if err := authority.sync(); err != nil {
		return fmt.Errorf("sync failed-publication rollback: %w", err)
	}
	return nil
}

func retainExactNamedObject(
	authority *outputRootAuthority,
	expected *stageDirectoryAuthority,
	name string,
) (*stageDirectoryAuthority, error) {
	if expected == nil {
		return nil, errors.New("exact directory identity is unavailable")
	}
	if expected.name == name && expected.verifyName(authority) == nil {
		return expected, nil
	}
	opened, err := authority.openRecoveryChildAuthority(name)
	if err != nil {
		return nil, err
	}
	fail := func(operationErr error) (*stageDirectoryAuthority, error) {
		return nil, errors.Join(operationErr, opened.close())
	}
	if err := expected.matchesAuthority(opened); err != nil {
		return fail(err)
	}
	leased, err := opened.tryAcquireRecoveryLease(authority)
	if err != nil {
		return fail(err)
	}
	if !leased {
		return fail(errors.New("failed publication is held by another lease"))
	}
	return opened, nil
}

func removeExactObjectAtName(
	authority *outputRootAuthority,
	expected *stageDirectoryAuthority,
	name string,
) (resultErr error) {
	directory, err := retainExactNamedObject(authority, expected, name)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.close()) }()
	return authority.removeRetainedChild(directory, nil)
}

func runPublicationValidation(validate func() error, boundary string) error {
	if validate == nil {
		return nil
	}
	if err := validate(); err != nil {
		return fmt.Errorf("validate publication source at %s: %w", boundary, err)
	}
	return nil
}

func reconcileExistingPublication(
	authority *outputRootAuthority,
	artifactDir *stageDirectoryAuthority,
	destinationDir *stageDirectoryAuthority,
	destination string,
	evidenceID string,
	budget EvidenceStoreBudget,
	transition func(string) error,
) (publication Publication, resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, destinationDir.close()) }()
	if err := verifyPublicationAuthorityWithBudget(destinationDir, evidenceID, budget); err != nil {
		return Publication{}, fmt.Errorf("content-addressed destination %s is invalid: %w", evidenceID, err)
	}
	seal, err := acquirePublicationSeal(destinationDir, budget)
	if err != nil {
		return Publication{}, fmt.Errorf("seal existing content-addressed destination %s: %w", evidenceID, err)
	}
	defer func() {
		if seal != nil {
			resultErr = errors.Join(resultErr, seal.Close())
		}
	}()
	if err := runPublicationTransition(transition, "existing-after-verification"); err != nil {
		return Publication{}, err
	}
	if err := errors.Join(seal.Verify(), destinationDir.verifyName(authority)); err != nil {
		return Publication{}, fmt.Errorf("content-addressed destination %s changed after verification: %w", evidenceID, err)
	}
	if err := removeExactObjectAtName(authority, artifactDir, artifactDir.name); err != nil {
		return Publication{}, fmt.Errorf("remove redundant verified stage: %w", err)
	}
	if err := runPublicationTransition(transition, "existing-after-stage-removal"); err != nil {
		return Publication{}, err
	}
	if err := errors.Join(seal.Verify(), destinationDir.verifyName(authority)); err != nil {
		return Publication{}, fmt.Errorf("content-addressed destination %s changed during stage cleanup: %w", evidenceID, err)
	}
	if err := authority.sync(); err != nil {
		return Publication{}, fmt.Errorf("sync evidence publication root: %w", err)
	}
	if err := runPublicationTransition(transition, "existing-after-root-sync"); err != nil {
		return Publication{}, err
	}
	if err := authority.verifyPath(); err != nil {
		return Publication{}, fmt.Errorf("verify output authority after reconciliation: %w", err)
	}
	if err := errors.Join(
		seal.Verify(),
		destinationDir.verifyName(authority),
		verifyPublicationAuthorityWithBudget(destinationDir, evidenceID, budget),
	); err != nil {
		return Publication{}, fmt.Errorf("finalize existing content-addressed destination %s: %w", evidenceID, err)
	}
	if err := seal.Close(); err != nil {
		seal = nil
		return Publication{}, fmt.Errorf("release existing publication seal: %w", err)
	}
	seal = nil
	return Publication{EvidenceID: evidenceID, Path: destination}, nil
}

func runPublicationTransition(transition func(string) error, name string) error {
	if transition == nil {
		return nil
	}
	if err := transition(name); err != nil {
		return fmt.Errorf("publication transition %s: %w", name, err)
	}
	return nil
}

func publicationPaths(outputRoot, stage string) (rootResult, stageResult string, resultErr error) {
	authority, err := openOutputRootAuthority(outputRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve evidence output root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, authority.close()) }()
	stagePath, err := filepath.Abs(stage)
	if err != nil {
		return "", "", fmt.Errorf("resolve evidence stage: %w", err)
	}
	if !samePath(filepath.Dir(stagePath), authority.path) {
		return "", "", errors.New("evidence stage must be a direct staging child of its output root")
	}
	info, err := os.Lstat(stagePath)
	if err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(filepath.Base(stagePath), ".staging-") || isReparsePointInfo(info) || !info.IsDir() {
		return "", "", errors.New("evidence stage must be a real direct staging child of its output root")
	}
	return authority.path, stagePath, nil
}

func publicationPathsWithAuthority(
	authority *outputRootAuthority,
	artifactDir *stageDirectoryAuthority,
) (string, string, error) {
	if authority == nil {
		return "", "", errors.New("evidence output authority is nil")
	}
	if err := authority.verifyPath(); err != nil {
		return "", "", err
	}
	if artifactDir == nil {
		return "", "", errors.New("evidence stage authority is nil")
	}
	artifactName := artifactDir.name
	if !strings.HasPrefix(artifactName, ".staging-") || filepath.Base(artifactName) != artifactName {
		return "", "", errors.New("evidence stage must be a direct staging child of its output root")
	}
	if err := artifactDir.verifyName(authority); err != nil {
		return "", "", err
	}
	stagePath := artifactDir.path
	info, err := os.Lstat(stagePath)
	if err != nil {
		return "", "", fmt.Errorf("inspect evidence stage: %w", err)
	}
	if isReparsePointInfo(info) || !info.IsDir() {
		return "", "", errors.New("evidence stage must be a real directory")
	}
	return authority.path, stagePath, nil
}

func resolveDirectoryAuthority(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func validRunID(runID string) bool {
	if len(runID) == 0 || len(runID) > 64 {
		return false
	}
	for _, character := range runID {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}

func recoverAbandonedStages(root string, now time.Time) error {
	return recoverAbandonedStagesWithBudget(root, now, DefaultEvidenceStoreBudget())
}

func recoverAbandonedStagesWithBudget(root string, now time.Time, budget EvidenceStoreBudget) error {
	authority, err := openOutputRootAuthority(root)
	if err != nil {
		return err
	}
	recoveryErr := recoverAbandonedStagesWithAuthorityAndBudget(authority, now, budget)
	return errors.Join(recoveryErr, authority.close())
}

func recoverAbandonedStagesWithAuthority(authority *outputRootAuthority, now time.Time) error {
	return recoverAbandonedStagesWithAuthorityAndBudget(authority, now, DefaultEvidenceStoreBudget())
}

func recoverAbandonedStagesWithAuthorityAndBudget(
	authority *outputRootAuthority,
	now time.Time,
	budget EvidenceStoreBudget,
) error {
	entries, err := authority.readDir()
	if err != nil {
		return fmt.Errorf("scan evidence stages: %w", err)
	}
	if err := preflightEvidenceRecovery(authority, entries, budget); err != nil {
		return fmt.Errorf("preflight evidence recovery: %w", err)
	}
	for _, entry := range entries {
		if err := recoverRuntimeStage(authority, entry.Name(), now, budget); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if err := recoverOrphanArtifactStage(authority, entry.Name(), now); err != nil {
			return err
		}
	}
	return nil
}

func preflightEvidenceRecovery(
	authority *outputRootAuthority,
	entries []os.DirEntry,
	budget EvidenceStoreBudget,
) error {
	meter, err := newEvidenceStoreMeter(budget)
	if err != nil {
		return err
	}
	if err := meter.observeRootEntries(len(entries)); err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		owned := strings.HasPrefix(name, ".runtime-") || strings.HasPrefix(name, ".staging-")
		if !owned {
			continue
		}
		directory, err := authority.openRecoveryChildAuthority(name)
		if authorityChildAbsent(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("retain evidence recovery candidate %s: %w", name, err)
		}
		walkErr := directory.walkEvidenceStore(&evidenceStoreWalk{meter: meter})
		if err := errors.Join(walkErr, directory.close()); err != nil {
			return fmt.Errorf("inventory evidence recovery candidate %s: %w", name, err)
		}
	}
	return nil
}

func recoverRuntimeStage(
	authority *outputRootAuthority,
	name string,
	now time.Time,
	budget EvidenceStoreBudget,
) (resultErr error) {
	if !strings.HasPrefix(name, ".runtime-") {
		return nil
	}
	runtimeDir, err := authority.openRecoveryChildAuthority(name)
	if err != nil {
		return fmt.Errorf("retain evidence runtime %s: %w", name, err)
	}
	defer func() { resultErr = errors.Join(resultErr, runtimeDir.close()) }()
	leased, err := runtimeDir.tryAcquireRecoveryLease(authority)
	if err != nil {
		return fmt.Errorf("lease evidence runtime %s: %w", name, err)
	}
	if !leased {
		return nil
	}
	modifiedAt, err := runtimeDir.modTime()
	if err != nil {
		return fmt.Errorf("inspect evidence runtime %s: %w", name, err)
	}
	runID := strings.TrimPrefix(name, ".runtime-")
	owner, validOwner, err := readStageOwnerAuthority(runtimeDir, runID, budget)
	if err != nil {
		return fmt.Errorf("read evidence runtime owner %s: %w", name, err)
	}
	if validOwner {
		matches, matchErr := processMatches(owner.ProcessID, owner.ProcessToken)
		if matchErr != nil || matches {
			// An unprovable owner is retained: cleanup must never trade disk
			// reclamation for deletion of another process's live stage.
			return nil
		}
	} else if now.Sub(modifiedAt) < abandonedStageMinimumAge {
		return nil
	}
	artifactName := ".staging-" + runID
	artifactDir, err := authority.openRecoveryChildAuthority(artifactName)
	if err == nil {
		defer func() { resultErr = errors.Join(resultErr, artifactDir.close()) }()
		artifactLeased, leaseErr := artifactDir.tryAcquireRecoveryLease(authority)
		if leaseErr != nil {
			return fmt.Errorf("lease evidence artifact %s: %w", artifactName, leaseErr)
		}
		if !artifactLeased {
			// The owner pathname can be forged or swapped. The artifact's own
			// kernel lease is the deletion authority for a live run.
			return nil
		}
		if err := authority.removeRetainedChild(artifactDir, nil); err != nil {
			return fmt.Errorf("recover abandoned artifact stage %s: %w", runID, err)
		}
	} else if !authorityChildAbsent(err) {
		return fmt.Errorf("retain evidence artifact %s: %w", artifactName, err)
	}
	if err := authority.removeRetainedChild(runtimeDir, nil); err != nil {
		return fmt.Errorf("recover abandoned runtime stage %s: %w", runID, err)
	}
	return nil
}

func readStageOwnerAuthority(
	runtimeDir *stageDirectoryAuthority,
	runID string,
	budget EvidenceStoreBudget,
) (stageOwner, bool, error) {
	meter, meterErr := newEvidenceStoreMeter(budget)
	if meterErr != nil {
		return stageOwner{}, false, meterErr
	}
	encoded, err := readAuthorityFileWithMeter(runtimeDir, stageOwnerName, evidenceMetadataFile, meter)
	if err != nil {
		if authorityChildAbsent(err) {
			return stageOwner{}, false, nil
		}
		return stageOwner{}, false, err
	}
	var owner stageOwner
	if json.Unmarshal(encoded, &owner) != nil {
		return stageOwner{}, false, nil
	}
	valid := owner.SchemaVersion == SchemaVersion && owner.RunID == runID && validRunID(owner.RunID) &&
		owner.ProcessID > 0 && owner.ProcessToken != "" && !owner.CreatedAt.IsZero()
	return owner, valid, nil
}

func recoverOrphanArtifactStage(
	authority *outputRootAuthority,
	name string,
	now time.Time,
) (resultErr error) {
	if !strings.HasPrefix(name, ".staging-") {
		return nil
	}
	artifactDir, err := authority.openRecoveryChildAuthority(name)
	if err != nil {
		if authorityChildAbsent(err) {
			return nil
		}
		return fmt.Errorf("retain orphan evidence stage %s: %w", name, err)
	}
	defer func() { resultErr = errors.Join(resultErr, artifactDir.close()) }()
	leased, err := artifactDir.tryAcquireRecoveryLease(authority)
	if err != nil {
		return fmt.Errorf("lease orphan evidence stage %s: %w", name, err)
	}
	if !leased {
		return nil
	}
	runID := strings.TrimPrefix(name, ".staging-")
	runtimeDir, err := authority.openRecoveryChildAuthority(".runtime-" + runID)
	if err == nil {
		return runtimeDir.close()
	}
	if !authorityChildAbsent(err) {
		return fmt.Errorf("retain orphan stage owner for %s: %w", runID, err)
	}
	modifiedAt, err := artifactDir.modTime()
	if err != nil {
		return fmt.Errorf("inspect orphan evidence stage %s: %w", name, err)
	}
	if now.Sub(modifiedAt) < abandonedStageMinimumAge {
		return nil
	}
	if err := authority.removeRetainedChild(artifactDir, nil); err != nil {
		return fmt.Errorf("recover orphan stage %s: %w", runID, err)
	}
	return nil
}

func removeOwnedTree(root, path string) error {
	authority, err := openOutputRootAuthority(root)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return errors.Join(err, authority.close())
	}
	if !samePath(filepath.Dir(path), authority.path) {
		return errors.Join(
			fmt.Errorf("refusing to remove unowned performance path %s", path), authority.close(),
		)
	}
	removeErr := removeOwnedTreeAuthority(authority, filepath.Base(path), nil)
	return errors.Join(removeErr, authority.close())
}

func removeOwnedTreeAuthority(
	authority *outputRootAuthority,
	name string,
	transition func(string) error,
) error {
	if authority == nil {
		return errors.New("evidence output authority is nil")
	}
	owned := strings.HasPrefix(name, ".staging-") || strings.HasPrefix(name, ".runtime-")
	if filepath.Base(name) != name || !owned {
		return fmt.Errorf("refusing to remove unowned performance child %s", name)
	}
	return authority.removeChild(name, transition)
}

func VerifyPublication(path, expectedID string) error {
	return VerifyPublicationWithBudget(path, expectedID, DefaultEvidenceStoreBudget())
}

func VerifyPublicationWithBudget(path, expectedID string, budget EvidenceStoreBudget) error {
	authority, err := openTreeAuthority(path)
	if err != nil {
		return err
	}
	return errors.Join(verifyPublicationAuthorityWithBudget(authority, expectedID, budget), authority.close())
}

func verifyPublicationAuthorityWithBudget(
	authority *stageDirectoryAuthority,
	expectedID string,
	budget EvidenceStoreBudget,
) error {
	meter, err := newEvidenceStoreMeter(budget)
	if err != nil {
		return err
	}
	manifestBytes, err := readAuthorityFileWithMeter(authority, manifestName, evidenceMetadataFile, meter)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest publicationManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	payload, err := readAuthorityFileWithMeter(authority, payloadName, evidencePayloadFile, meter)
	if err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	computedID := hashBytes(payload)
	if manifest.SchemaVersion != SchemaVersion || manifest.PayloadFile != payloadName ||
		manifest.EvidenceID != computedID || manifest.PayloadSHA256 != computedID ||
		(expectedID != "" && expectedID != computedID) {
		return errors.New("evidence manifest does not match its canonical payload")
	}
	var evidence Evidence
	if err := json.Unmarshal(payload, &evidence); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if evidence.SchemaVersion != SchemaVersion || evidence.Kind != EvidenceKind {
		return errors.New("evidence payload has an unsupported contract")
	}
	expected := make(map[string]ArtifactFile, len(evidence.Artifacts))
	for _, artifact := range evidence.Artifacts {
		if _, duplicate := expected[artifact.Path]; duplicate {
			return fmt.Errorf("evidence repeats artifact %s", artifact.Path)
		}
		expected[artifact.Path] = artifact
	}
	actual, err := inspectArtifactsAuthorityWithMeter(
		authority, meter, map[string]struct{}{manifestName: {}, payloadName: {}},
	)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("evidence has %d artifact files; manifest records %d", len(actual), len(expected))
	}
	for _, artifact := range actual {
		if expected[artifact.Path] != artifact {
			return fmt.Errorf("artifact %s differs from its manifest", artifact.Path)
		}
	}
	if err := verifyEvidenceArtifactBindings(evidence, expected); err != nil {
		return err
	}
	return nil
}

func verifyEvidenceArtifactBindings(evidence Evidence, artifacts map[string]ArtifactFile) error {
	produced := make(map[string]string)
	for _, workload := range evidence.Workloads {
		workloadID := workload.Definition.ID
		if err := verifyCommandArtifactBindings(
			workload.Build, EvidencePhaseBuild, evidence.Status,
			"workload "+workloadID+" build", artifacts, produced,
		); err != nil {
			return err
		}

		binary := artifactFromBinary(workload.Binary)
		binaryPresent := workload.Binary != (BinaryEvidence{})
		if !binaryPresent {
			if workload.Build.Outcome != EvidenceOutcomeFailed || evidence.Status != string(EvidenceOutcomeFailed) {
				return fmt.Errorf("workload %s omits its binary outside a failed build", workloadID)
			}
			if len(workload.Samples) != 0 || workload.Profile != nil {
				return fmt.Errorf("workload %s continued after a failed build without a binary", workloadID)
			}
		} else {
			if err := verifyBoundArtifact(binary, artifacts, "workload "+workloadID+" binary", true); err != nil {
				return err
			}
			if workload.Binary.GoBuildID == "" || workload.Binary.GoVersionMetadata == "" ||
				!validSHA256(workload.Binary.BuildGraphSHA256) {
				return fmt.Errorf("workload %s binary omits its reproducible build identity", workloadID)
			}
			if !commandClaimsArtifact(workload.Build, binary) {
				return fmt.Errorf("workload %s build does not claim its binary artifact", workloadID)
			}
		}
		if workload.Build.Outcome == EvidenceOutcomeSucceeded && !binaryPresent {
			return fmt.Errorf("workload %s successful build has no binary identity", workloadID)
		}

		for index, sample := range workload.Samples {
			if err := verifyCommandArtifactBindings(
				sample.Command, EvidencePhaseSample, evidence.Status,
				fmt.Sprintf("workload %s sample %d", workloadID, index+1), artifacts, produced,
			); err != nil {
				return err
			}
		}
		if workload.Profile == nil {
			continue
		}
		profile := workload.Profile
		if err := verifyCommandArtifactBindings(
			profile.Command, EvidencePhaseProfile, evidence.Status,
			"workload "+workloadID+" profile", artifacts, produced,
		); err != nil {
			return err
		}
		for index, verification := range profile.Verification {
			if err := verifyCommandArtifactBindings(
				verification, EvidencePhaseProfileVerification, evidence.Status,
				fmt.Sprintf("workload %s profile verification %d", workloadID, index+1), artifacts, produced,
			); err != nil {
				return err
			}
		}
		profileBinaryPresent := profile.Binary != (ArtifactFile{})
		cpuPresent := profile.CPU != (ArtifactFile{})
		memoryPresent := profile.Memory != (ArtifactFile{})
		if profileBinaryPresent {
			if !binaryPresent || profile.Binary != binary {
				return fmt.Errorf("workload %s profile names a different binary identity", workloadID)
			}
			if err := verifyBoundArtifact(profile.Binary, artifacts, "workload "+workloadID+" profile binary", true); err != nil {
				return err
			}
		}
		for name, identity := range map[string]ArtifactFile{"CPU": profile.CPU, "memory": profile.Memory} {
			if identity == (ArtifactFile{}) {
				continue
			}
			if err := verifyBoundArtifact(
				identity, artifacts, fmt.Sprintf("workload %s %s profile", workloadID, name), true,
			); err != nil {
				return err
			}
		}
		if profile.Command.Outcome == EvidenceOutcomeSucceeded {
			if !profileBinaryPresent || !cpuPresent || !memoryPresent {
				return fmt.Errorf("workload %s successful profile omits a required identity", workloadID)
			}
			if profile.CPU.Path == profile.Memory.Path || profile.CPU.Path == binary.Path || profile.Memory.Path == binary.Path {
				return fmt.Errorf("workload %s profile identities are not distinct", workloadID)
			}
			if !commandClaimsArtifact(profile.Command, profile.CPU) ||
				!commandClaimsArtifact(profile.Command, profile.Memory) {
				return fmt.Errorf("workload %s profile command does not claim both profile artifacts", workloadID)
			}
		}
	}
	return nil
}

func verifyCommandArtifactBindings(
	command CommandEvidence,
	expectedPhase EvidencePhase,
	evidenceStatus string,
	label string,
	artifacts map[string]ArtifactFile,
	produced map[string]string,
) error {
	if command.Phase != expectedPhase {
		return fmt.Errorf("%s has phase %q, want %q", label, command.Phase, expectedPhase)
	}
	switch command.Outcome {
	case EvidenceOutcomeSucceeded:
		if command.Error != "" {
			return fmt.Errorf("%s succeeded with an error diagnostic", label)
		}
	case EvidenceOutcomeFailed:
		if strings.TrimSpace(command.Error) == "" {
			return fmt.Errorf("%s failed without an error diagnostic", label)
		}
		if evidenceStatus != string(EvidenceOutcomeFailed) {
			return fmt.Errorf("%s failed inside non-failed evidence", label)
		}
	default:
		return fmt.Errorf("%s has unsupported outcome %q", label, command.Outcome)
	}
	for _, identity := range command.Artifacts {
		if err := verifyBoundArtifact(identity, artifacts, label+" artifact", false); err != nil {
			return err
		}
		if previous, duplicate := produced[identity.Path]; duplicate {
			return fmt.Errorf("artifact %s is claimed by both %s and %s", identity.Path, previous, label)
		}
		produced[identity.Path] = label
	}
	return nil
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

func inspectArtifacts(root string) ([]ArtifactFile, error) {
	authority, err := openTreeAuthority(root)
	if err != nil {
		return nil, err
	}
	artifacts, inspectErr := inspectArtifactsAuthority(authority)
	return artifacts, errors.Join(inspectErr, authority.close())
}

func inspectArtifactsAuthority(authority *stageDirectoryAuthority) ([]ArtifactFile, error) {
	return inspectArtifactsAuthorityWithBudget(authority, DefaultEvidenceStoreBudget())
}

func inspectArtifactsAuthorityWithBudget(
	authority *stageDirectoryAuthority,
	budget EvidenceStoreBudget,
) ([]ArtifactFile, error) {
	meter, err := newEvidenceStoreMeter(budget)
	if err != nil {
		return nil, err
	}
	return inspectArtifactsAuthorityWithMeter(authority, meter, nil)
}

func inspectArtifactsAuthorityWithMeter(
	authority *stageDirectoryAuthority,
	meter *evidenceStoreMeter,
	skipFiles map[string]struct{},
) ([]ArtifactFile, error) {
	var artifacts []ArtifactFile
	err := authority.walkEvidenceStore(&evidenceStoreWalk{meter: meter, skipFiles: skipFiles, visitor: func(
		relative string, file *os.File, info os.FileInfo,
	) error {
		if relative == payloadName || relative == manifestName {
			return nil
		}
		identity, err := artifactIdentityFromOpenFile(file, info, relative)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, identity)
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("inspect evidence artifacts: %w", err)
	}
	sort.Slice(artifacts, func(left, right int) bool { return artifacts[left].Path < artifacts[right].Path })
	return artifacts, nil
}

func readAuthorityFileWithMeter(
	authority *stageDirectoryAuthority,
	name string,
	class evidenceStoreFileClass,
	meter *evidenceStoreMeter,
) (content []byte, resultErr error) {
	file, info, err := authority.openRegularFile(name)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err := meter.observeFile(name, 1, info.Size(), class); err != nil {
		return nil, err
	}
	first := make([]byte, int(info.Size()))
	if _, err := io.ReadFull(file, first); err != nil {
		return nil, err
	}
	if err := requireAuthorityFileEOF(file, name); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	buffer := make([]byte, evidenceFileReadBatch)
	for offset := 0; offset < len(first); {
		length := min(len(first)-offset, len(buffer))
		chunk := buffer[:length]
		if _, err := io.ReadFull(file, chunk); err != nil {
			return nil, err
		}
		if !bytes.Equal(first[offset:offset+length], chunk) {
			return nil, fmt.Errorf("artifact %s changed while it was read", name)
		}
		offset += length
	}
	if err := requireAuthorityFileEOF(file, name); err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, after) || info.Size() != int64(len(first)) {
		return nil, fmt.Errorf("artifact %s changed while it was read", name)
	}
	return first, nil
}

func requireAuthorityFileEOF(file *os.File, name string) error {
	var extra [1]byte
	if _, err := file.Read(extra[:]); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("artifact %s grew while it was read", name)
		}
		return err
	}
	return nil
}

func artifactIdentityFromOpenFile(file *os.File, info os.FileInfo, relative string) (ArtifactFile, error) {
	if info.Size() < 0 {
		return ArtifactFile{}, fmt.Errorf("artifact %s has a negative size", relative)
	}
	hashes := make([]string, 0, 2)
	for range 2 {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return ArtifactFile{}, err
		}
		digest := sha256.New()
		if _, err := io.CopyN(digest, file, info.Size()); err != nil {
			return ArtifactFile{}, err
		}
		if err := requireAuthorityFileEOF(file, relative); err != nil {
			return ArtifactFile{}, err
		}
		hashes = append(hashes, hex.EncodeToString(digest.Sum(nil)))
	}
	after, err := file.Stat()
	if err != nil {
		return ArtifactFile{}, err
	}
	if !os.SameFile(info, after) || info.Size() != after.Size() || hashes[0] != hashes[1] {
		return ArtifactFile{}, fmt.Errorf("artifact %s changed while it was hashed", relative)
	}
	return ArtifactFile{Path: filepath.ToSlash(relative), Bytes: after.Size(), SHA256: hashes[0]}, nil
}
