package perfevidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maximumRepositoryContainmentObjects = 250_000

type directoryIdentity struct {
	volume uint64
	object uint64
}

func validateEvidenceOutputRoot(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	outputRoot string,
	runID string,
) error {
	authority, err := openOutputRootAuthority(outputRoot)
	if err != nil {
		return err
	}
	validationErr := validateEvidenceOutputAuthority(ctx, runner, repositoryRoot, authority, runID)
	return errors.Join(validationErr, authority.close())
}

func validateEvidenceOutputAuthority(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	authority *outputRootAuthority,
	runID string,
) error {
	if !validRunID(runID) {
		return fmt.Errorf("performance run ID %q is not path-safe", runID)
	}
	outputRelative, inside, repository, err := physicalRepositoryRelative(repositoryRoot, authority)
	if err != nil {
		return fmt.Errorf("validate evidence output authority: %w", err)
	}
	if !inside {
		return nil
	}
	if outputRelative == "." {
		return errors.New("evidence output root must not be the repository root")
	}
	// Git's ignore contract is attached to the exact retained output object.
	// Physical identity traversal makes aliases such as SUBST drives and bind
	// mounts obey the same policy as their repository-native spelling.
	probes := []string{
		filepath.ToSlash(outputRelative) + "/",
		filepath.Join(outputRelative, ".staging-"+runID, snapshotDirectoryName, "workspace", "go.mod"),
		filepath.Join(outputRelative, ".runtime-"+runID, stageOwnerName),
		filepath.Join(outputRelative, strings.Repeat("a", 64), payloadName),
	}
	for _, probe := range probes {
		result, runErr := runner.Run(ctx, Command{
			Executable: "git",
			Arguments: []string{
				"-C", repository, "check-ignore", "--quiet", "--no-index", "--", filepath.ToSlash(probe),
			},
		})
		if result.ExitCode == 1 {
			return fmt.Errorf(
				"evidence output root inside the repository must be Git-ignored; %s is visible to Git",
				filepath.ToSlash(probe),
			)
		}
		if runErr != nil || result.ExitCode != 0 {
			return commandFailure("verify evidence output ignore authority", result, runErr)
		}
	}
	return authority.verifyPath()
}

func physicalRepositoryRelative(
	repositoryRoot string,
	authority *outputRootAuthority,
) (string, bool, string, error) {
	if authority == nil {
		return "", false, "", errors.New("evidence output authority is nil")
	}
	if err := authority.verifyPath(); err != nil {
		return "", false, "", err
	}
	repository, err := resolveDirectoryAuthority(repositoryRoot)
	if err != nil {
		return "", false, "", fmt.Errorf("resolve repository authority: %w", err)
	}
	repositoryDirectories, repositoryIdentity, err := repositoryDirectoryIdentities(repository)
	if err != nil {
		return "", false, "", fmt.Errorf("index repository directory authorities: %w", err)
	}
	current := authority.path
	var components []string
	for {
		identity, identityErr := directoryIdentityAt(current)
		if identityErr != nil {
			return "", false, "", fmt.Errorf("identify output ancestor %s: %w", current, identityErr)
		}
		if repositoryRelative, found := repositoryDirectories[identity]; found {
			relativeParts := append([]string(nil), components...)
			if repositoryRelative != "." {
				relativeParts = append([]string{repositoryRelative}, relativeParts...)
			}
			relative := "."
			if len(relativeParts) != 0 {
				relative = filepath.Join(relativeParts...)
			}
			stable, stableErr := directoryIdentityAt(repository)
			if stableErr != nil || stable != repositoryIdentity {
				return "", false, "", errors.New("repository authority changed during containment validation")
			}
			return relative, true, repository, authority.verifyPath()
		}
		parent := filepath.Dir(current)
		if samePath(parent, current) {
			return "", false, repository, authority.verifyPath()
		}
		components = append([]string{filepath.Base(current)}, components...)
		current = parent
	}
}

func repositoryDirectoryIdentities(
	repository string,
) (map[directoryIdentity]string, directoryIdentity, error) {
	rootIdentity, err := directoryIdentityAt(repository)
	if err != nil {
		return nil, directoryIdentity{}, err
	}
	identities := make(map[directoryIdentity]string)
	err = walkBoundedTree(
		repository, maximumRepositoryContainmentObjects, maximumSnapshotInputDepth,
		func(path, relative string, info os.FileInfo) (bool, error) {
			if isReparsePointInfo(info) {
				return false, nil
			}
			if !info.IsDir() {
				return false, nil
			}
			identity, err := directoryIdentityAt(path)
			if err != nil {
				return false, err
			}
			if _, duplicate := identities[identity]; duplicate && !samePath(path, repository) {
				// Bind-mounted cycles and aliases cannot add a new containment
				// boundary after their inode has already been indexed.
				return false, nil
			}
			identities[identity] = relative
			return true, nil
		})
	if err != nil {
		return nil, directoryIdentity{}, err
	}
	stable, err := directoryIdentityAt(repository)
	if err != nil || stable != rootIdentity {
		return nil, directoryIdentity{}, errors.Join(
			errors.New("repository authority changed during directory identity indexing"), err,
		)
	}
	return identities, rootIdentity, nil
}

func writeExclusive(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, err := file.Write(content)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	if written != len(content) {
		return errors.Join(
			fmt.Errorf("short evidence write: wrote %d of %d bytes", written, len(content)), file.Close(),
		)
	}
	return file.Close()
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

type preparedMutationOutput struct {
	path string
	sink mutationOutputSink
}

func validateMutationOutputs(intent mutationIntent, outputs []MutationOutput) error {
	switch intent {
	case mutationIntentVerification:
		if len(outputs) != 0 {
			return errors.New("private verification command cannot publish protected outputs")
		}
		return nil
	case mutationIntentArtifactProduction:
		if len(outputs) == 0 {
			return errors.New("artifact-producing mutation requires at least one protected output")
		}
	default:
		return errors.New("private command mutation intent is unsupported")
	}
	if len(outputs) > maximumProtectedOutputCount {
		return fmt.Errorf(
			"protected output count must be in [1, %d]",
			maximumProtectedOutputCount,
		)
	}
	seen := make(map[string]string, len(outputs))
	var aggregate int64
	for _, output := range outputs {
		if output.HostPath == "" || !filepath.IsAbs(output.HostPath) || filepath.Clean(output.HostPath) != output.HostPath {
			return fmt.Errorf("protected output path %q is not canonical and absolute", output.HostPath)
		}
		if output.MaxBytes < 1 || output.MaxBytes > maximumProtectedOutputBytes {
			return fmt.Errorf(
				"protected output %s max bytes must be in [1, %d]",
				output.HostPath,
				maximumProtectedOutputBytes,
			)
		}
		if aggregate > int64(maximumProtectedOutputAggregateBytes)-output.MaxBytes {
			return fmt.Errorf(
				"protected outputs exceed aggregate byte limit %d",
				maximumProtectedOutputAggregateBytes,
			)
		}
		aggregate += output.MaxBytes
		parent, err := filepath.EvalSymlinks(filepath.Dir(output.HostPath))
		if err != nil {
			return fmt.Errorf("resolve protected output parent %s: %w", output.HostPath, err)
		}
		parentInfo, err := os.Stat(parent)
		if err != nil || !parentInfo.IsDir() {
			return errors.Join(fmt.Errorf("protected output parent %s is not a directory", parent), err)
		}
		key := platformPathKey(filepath.Join(parent, filepath.Base(output.HostPath)))
		if previous, duplicate := seen[key]; duplicate {
			return fmt.Errorf("protected output path %s aliases duplicate %s", output.HostPath, previous)
		}
		seen[key] = output.HostPath
	}
	return nil
}

func adoptMutationOutputGroup(
	prepared []preparedMutationOutput,
) (map[string]byteConsumptionAuthority, error) {
	authorities := make(map[string]byteConsumptionAuthority, len(prepared))
	ordered := make([]byteConsumptionAuthority, 0, len(prepared))
	for _, output := range prepared {
		authority, err := output.sink.adopt()
		if err != nil {
			return nil, fmt.Errorf("adopt protected command output %s: %w", output.path, err)
		}
		if authority == nil {
			return nil, fmt.Errorf("adopt protected command output %s: retained authority is unavailable", output.path)
		}
		authorities[output.path] = authority
		ordered = append(ordered, authority)
	}
	if err := verifyConsumptionAuthorities(ordered); err != nil {
		return nil, fmt.Errorf("verify protected output group before publication: %w", err)
	}
	// finalize is deliberately infallible: every OS operation that can fail has
	// already completed while all members are still rollback-capable.
	for _, output := range prepared {
		output.sink.finalize()
	}
	return authorities, nil
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
