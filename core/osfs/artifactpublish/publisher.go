// Package artifactpublish atomically installs immutable artifact generations
// through the same handle-owned native namespace used by resumable output.
package artifactpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const (
	maximumArtifactCount = 8
	maximumArtifactBytes = 16 << 20
	maximumTotalBytes    = 32 << 20
	maximumNameBytes     = 255
)

var (
	// ErrCollision reports that the immutable final name already exists.
	ErrCollision = errors.New("artifact publication destination already exists")
	// ErrUnsafe reports that exact namespace or byte authority was lost.
	ErrUnsafe = errors.New("artifact publication authority is unsafe")
)

// Artifact binds one relative name to exact bytes and their caller-authorized digest.
type Artifact struct {
	Name   string
	Bytes  []byte
	SHA256 string
}

// DirectoryRequest publishes all artifacts through one no-replace directory install.
type DirectoryRequest struct {
	ParentPath  string
	OutputName  string
	StagingName string
	Artifacts   []Artifact
}

// FileRequest publishes one artifact through a no-replace native hard link.
type FileRequest struct {
	ParentPath  string
	OutputName  string
	StagingName string
	Artifact    Artifact
}

// PublishedArtifact contains bytes reread from the durable final namespace.
type PublishedArtifact struct {
	Name   string
	Bytes  []byte
	SHA256 string
}

// Result is returned only after final-name identity and byte verification.
type Result struct {
	Artifacts []PublishedArtifact
}

type platformOpener func(string, bool) (outputcap.Platform, error)

type publicationBoundary uint8

const (
	boundaryBeforeCommit publicationBoundary = iota + 1
	boundaryBeforeNativeCommit
	boundaryAfterCommit
	boundaryAfterDurability
)

type transactionState struct {
	root        outputcap.Directory
	stage       outputcap.Directory
	installed   outputcap.Directory
	stagedFiles []stagedArtifact
	stageName   string
	outputName  string
}

type transactionHook func(publicationBoundary, *transactionState) error

type publisher struct {
	open                  platformOpener
	openPrivate           platformOpener
	hook                  transactionHook
	prepareSettlementHook func() error
}

type stagedArtifact struct {
	expected       Artifact
	file           outputcap.File
	closedIdentity outputcap.TransientFileIdentity
}

// PublishDirectory installs one immutable generation without replacing any final entry.
func PublishDirectory(request DirectoryRequest) (Result, error) {
	return publisher{open: openNativePlatform}.publishDirectory(request)
}

// PublishFile installs one immutable file without replacing any final entry.
func PublishFile(request FileRequest) (Result, error) {
	return publisher{open: openNativePlatform}.publishFile(request)
}

func (owner publisher) publishDirectory(request DirectoryRequest) (result Result, resultErr error) {
	normalized, err := normalizeDirectoryRequest(request)
	if err != nil {
		return Result{}, err
	}
	platform, root, err := owner.openRoot(normalized.ParentPath)
	if err != nil {
		return Result{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()

	stage, err := root.CreateDirectory(normalized.StagingName, true)
	if err != nil {
		return Result{}, classifyNamespaceError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, stage.Close()) }()
	state := &transactionState{
		root: root, stage: stage, stageName: normalized.StagingName, outputName: normalized.OutputName,
	}
	for _, artifact := range normalized.Artifacts {
		file, createErr := stage.CreateFile(artifact.Name, true, int64(len(artifact.Bytes)))
		if createErr != nil {
			return Result{}, classifyNamespaceError(createErr)
		}
		state.stagedFiles = append(state.stagedFiles, stagedArtifact{expected: artifact, file: file})
		defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
		if writeErr := writeAndVerify(file, artifact); writeErr != nil {
			return Result{}, writeErr
		}
	}
	if err := verifyStagedDirectory(state); err != nil {
		return Result{}, err
	}
	if err := owner.cross(boundaryBeforeCommit, state); err != nil {
		return Result{}, err
	}
	// A second verification after the hostile boundary is the last userspace
	// cut before the native handle-relative no-replace install.
	if err := verifyStagedDirectory(state); err != nil {
		return Result{}, err
	}
	if err := prepareDirectoryCommit(state.stagedFiles); err != nil {
		return Result{}, err
	}
	if err := owner.cross(boundaryBeforeNativeCommit, state); err != nil {
		return Result{}, err
	}
	installed, err := root.InstallDirectoryNoReplace(stage, normalized.OutputName)
	if err != nil {
		return Result{}, classifyNamespaceError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, installed.Close()) }()
	state.installed = installed
	if err := owner.cross(boundaryAfterCommit, state); err != nil {
		return Result{}, err
	}
	if err := syncDirectoryGeneration(root, installed, state.stagedFiles); err != nil {
		return Result{}, err
	}
	if err := owner.cross(boundaryAfterDurability, state); err != nil {
		return Result{}, err
	}
	return owner.reopenDirectoryResult(normalized.ParentPath, normalized.OutputName, root, installed, state.stagedFiles)
}

func (owner publisher) publishFile(request FileRequest) (result Result, resultErr error) {
	normalized, err := normalizeFileRequest(request)
	if err != nil {
		return Result{}, err
	}
	platform, root, err := owner.openRoot(normalized.ParentPath)
	if err != nil {
		return Result{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	file, err := root.CreateFile(normalized.StagingName, true, int64(len(normalized.Artifact.Bytes)))
	if err != nil {
		return Result{}, classifyNamespaceError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	state := &transactionState{
		root: root, stageName: normalized.StagingName, outputName: normalized.OutputName,
		stagedFiles: []stagedArtifact{{expected: normalized.Artifact, file: file}},
	}
	if err := writeAndVerify(file, normalized.Artifact); err != nil {
		return Result{}, err
	}
	if err := verifyStagedFile(root, normalized.StagingName, state.stagedFiles[0]); err != nil {
		return Result{}, err
	}
	if err := owner.cross(boundaryBeforeCommit, state); err != nil {
		return Result{}, err
	}
	if err := verifyStagedFile(root, normalized.StagingName, state.stagedFiles[0]); err != nil {
		return Result{}, err
	}
	installed, err := root.LinkFileNoReplace(file, normalized.OutputName)
	if err != nil {
		return Result{}, classifyNamespaceError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, installed.Close()) }()
	if err := owner.cross(boundaryAfterCommit, state); err != nil {
		return Result{}, err
	}
	// The writable staged handle identifies the same inode as the installed hard
	// link. Syncing it avoids depending on whether a platform grants flush access
	// to the read-only handle returned by the no-replace link operation.
	if err := file.Sync(); err != nil {
		return Result{}, unsafeError("sync installed artifact file", err)
	}
	if err := root.Sync(); err != nil {
		return Result{}, unsafeError("sync artifact publication parent", err)
	}
	if err := owner.cross(boundaryAfterDurability, state); err != nil {
		return Result{}, err
	}
	result, err = owner.reopenFileResult(normalized.ParentPath, normalized.OutputName, root, file, normalized.Artifact)
	if err != nil {
		return Result{}, err
	}
	// The private staging link is retired only after final authority is proven;
	// a failed or ambiguous transaction intentionally leaves it for diagnosis.
	if err := root.RemoveFile(normalized.StagingName, file); err != nil {
		return Result{}, unsafeError("retire proven artifact staging link", err)
	}
	if err := root.Sync(); err != nil {
		return Result{}, unsafeError("sync retired artifact staging link", err)
	}
	return owner.reopenFileResult(normalized.ParentPath, normalized.OutputName, root, file, normalized.Artifact)
}

func (owner publisher) openRoot(parentPath string) (
	outputcap.Platform,
	outputcap.Directory,
	error,
) {
	return owner.openPublicationRoot(owner.open, parentPath, false)
}

func (owner publisher) openPrivateRoot(parentPath string) (
	outputcap.Platform,
	outputcap.Directory,
	error,
) {
	return owner.openPublicationRoot(owner.openPrivate, parentPath, false)
}

func (owner publisher) openOrCreatePrivateRoot(parentPath string) (
	outputcap.Platform,
	outputcap.Directory,
	error,
) {
	return owner.openPublicationRoot(owner.openPrivate, parentPath, true)
}

func (owner publisher) openPublicationRoot(opener platformOpener, parentPath string, create bool) (
	outputcap.Platform,
	outputcap.Directory,
	error,
) {
	if opener == nil {
		return nil, nil, errors.Join(ErrUnsafe, errors.New("artifact publication root opener is absent"))
	}
	platform, err := opener(parentPath, create)
	if err != nil {
		return nil, nil, unsafeError("open artifact publication parent", err)
	}
	root := platform.Root()
	if root == nil {
		return nil, nil, errors.Join(ErrUnsafe, platform.Close())
	}
	// The platform root is the publication authority: every mutation below it is
	// handle-relative, and a separately opened root must match it before success
	// is returned. The resumable-output public-operation guard carries unrelated
	// recovery policy and would couple this immutable artifact transaction to a
	// state machine it does not use.
	return platform, root, nil
}

func (owner publisher) reopenDirectoryResult(
	parentPath string,
	outputName string,
	originalRoot outputcap.Directory,
	installed outputcap.Directory,
	staged []stagedArtifact,
) (result Result, resultErr error) {
	platform, root, err := owner.openRoot(parentPath)
	if err != nil {
		return Result{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	sameRoot, err := originalRoot.SameDirectory(root)
	if err != nil || !sameRoot {
		return Result{}, unsafeError("reopen exact artifact publication parent", err)
	}
	final, err := root.OpenDirectory(outputName, true)
	if err != nil {
		return Result{}, unsafeError("reopen published artifact directory", err)
	}
	defer func() { resultErr = errors.Join(resultErr, final.Close()) }()
	sameDirectory, err := installed.SameDirectory(final)
	if err != nil || !sameDirectory {
		return Result{}, unsafeError("verify published artifact directory identity", err)
	}
	return verifyDirectoryArtifacts(final, staged)
}

func (owner publisher) reopenFileResult(
	parentPath string,
	outputName string,
	originalRoot outputcap.Directory,
	originalFile outputcap.File,
	expected Artifact,
) (result Result, resultErr error) {
	platform, root, err := owner.openRoot(parentPath)
	if err != nil {
		return Result{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	sameRoot, err := originalRoot.SameDirectory(root)
	if err != nil || !sameRoot {
		return Result{}, unsafeError("reopen exact artifact file parent", err)
	}
	final, err := root.OpenFile(outputName, true, false)
	if err != nil {
		return Result{}, unsafeError("reopen published artifact file", err)
	}
	defer func() { resultErr = errors.Join(resultErr, final.Close()) }()
	sameFile, err := originalFile.SameFile(final)
	if err != nil || !sameFile {
		return Result{}, unsafeError("verify published artifact file identity", err)
	}
	actual, err := readAndVerify(final, expected)
	if err != nil {
		return Result{}, err
	}
	return Result{Artifacts: []PublishedArtifact{actual}}, nil
}

func verifyStagedDirectory(state *transactionState) error {
	named, err := state.root.OpenDirectory(state.stageName, true)
	if err != nil {
		return unsafeError("reopen staged artifact directory", err)
	}
	defer func() { _ = named.Close() }()
	same, err := state.stage.SameDirectory(named)
	if err != nil || !same {
		return unsafeError("verify staged artifact directory identity", err)
	}
	_, err = verifyDirectoryArtifacts(named, state.stagedFiles)
	return err
}

func verifyDirectoryArtifacts(directory outputcap.Directory, staged []stagedArtifact) (result Result, resultErr error) {
	names, err := directory.Names(len(staged) + 1)
	if err != nil {
		return Result{}, unsafeError("enumerate exact artifact directory", err)
	}
	expectedNames := make([]string, 0, len(staged))
	for _, item := range staged {
		expectedNames = append(expectedNames, item.expected.Name)
	}
	slices.Sort(expectedNames)
	if !slices.Equal(names, expectedNames) {
		return Result{}, unsafeError("verify exact artifact directory entries", nil)
	}
	published := make([]PublishedArtifact, 0, len(staged))
	for _, item := range staged {
		opened, openErr := directory.OpenFile(item.expected.Name, true, false)
		if openErr != nil {
			return Result{}, unsafeError("reopen staged artifact file", openErr)
		}
		same, compareErr := stagedFileMatches(item, opened)
		if compareErr != nil || !same {
			_ = opened.Close()
			return Result{}, unsafeError("verify staged artifact file identity", compareErr)
		}
		actual, readErr := readAndVerify(opened, item.expected)
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			return Result{}, errors.Join(readErr, unsafeError("close verified artifact file", closeErr))
		}
		published = append(published, actual)
	}
	return Result{Artifacts: published}, nil
}

func verifyStagedFile(root outputcap.Directory, name string, staged stagedArtifact) (resultErr error) {
	opened, err := root.OpenFile(name, true, false)
	if err != nil {
		return unsafeError("reopen staged artifact file", err)
	}
	defer func() { resultErr = errors.Join(resultErr, opened.Close()) }()
	same, err := staged.file.SameFile(opened)
	if err != nil || !same {
		return unsafeError("verify staged artifact file identity", err)
	}
	_, err = readAndVerify(opened, staged.expected)
	return err
}

func syncDirectoryGeneration(root, installed outputcap.Directory, staged []stagedArtifact) error {
	for _, item := range staged {
		opened, err := installed.OpenFile(item.expected.Name, true, true)
		if err != nil {
			return unsafeError("reopen published artifact file for durability", err)
		}
		same, compareErr := stagedFileMatches(item, opened)
		if compareErr != nil || !same {
			_ = opened.Close()
			return unsafeError("verify published artifact file before durability", compareErr)
		}
		if _, err := readAndVerify(opened, item.expected); err != nil {
			_ = opened.Close()
			return err
		}
		if err := opened.Sync(); err != nil {
			_ = opened.Close()
			return unsafeError("sync published artifact file", err)
		}
		if err := opened.Close(); err != nil {
			return unsafeError("close synced artifact file", err)
		}
	}
	if err := installed.Sync(); err != nil {
		return unsafeError("sync published artifact directory", err)
	}
	if err := root.Sync(); err != nil {
		return unsafeError("sync artifact publication parent", err)
	}
	return nil
}

func stagedFileMatches(staged stagedArtifact, opened outputcap.File) (bool, error) {
	if staged.file != nil {
		return staged.file.SameFile(opened)
	}
	provider, ok := opened.(outputcap.CloseRevalidationIdentityProvider)
	if !ok || staged.closedIdentity.IsZero() {
		return false, unsafeError("recover closed staged file identity", nil)
	}
	actual, err := provider.CloseRevalidationIdentity()
	if err != nil {
		return false, err
	}
	return staged.closedIdentity.Equal(actual), nil
}

func writeAndVerify(file outputcap.File, artifact Artifact) error {
	written, err := file.WriteAt(artifact.Bytes, 0)
	if err != nil || written != len(artifact.Bytes) {
		return unsafeError("write exact staged artifact bytes", errors.Join(err, io.ErrShortWrite))
	}
	if err := file.Sync(); err != nil {
		return unsafeError("sync staged artifact file", err)
	}
	_, err = readAndVerify(file, artifact)
	return err
}

func readAndVerify(file outputcap.File, expected Artifact) (PublishedArtifact, error) {
	size, err := file.Size()
	if err != nil || size != uint64(len(expected.Bytes)) {
		return PublishedArtifact{}, unsafeError("verify exact artifact size", err)
	}
	actual := make([]byte, len(expected.Bytes))
	read, err := file.ReadAt(actual, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return PublishedArtifact{}, unsafeError("read exact artifact bytes", err)
	}
	if read != len(actual) || !slices.Equal(actual, expected.Bytes) {
		return PublishedArtifact{}, unsafeError("verify exact artifact bytes", nil)
	}
	digest := sha256.Sum256(actual)
	encodedDigest := hex.EncodeToString(digest[:])
	if encodedDigest != expected.SHA256 {
		return PublishedArtifact{}, unsafeError("verify exact artifact digest", nil)
	}
	return PublishedArtifact{Name: expected.Name, Bytes: actual, SHA256: encodedDigest}, nil
}

func (owner publisher) cross(boundary publicationBoundary, state *transactionState) error {
	if owner.hook == nil {
		return nil
	}
	if err := owner.hook(boundary, state); err != nil {
		return unsafeError("cross artifact publication boundary", err)
	}
	return nil
}

func normalizeDirectoryRequest(request DirectoryRequest) (DirectoryRequest, error) {
	if len(request.Artifacts) < 1 || len(request.Artifacts) > maximumArtifactCount {
		return DirectoryRequest{}, fmt.Errorf("%w: artifact count is outside the safe range", ErrUnsafe)
	}
	parent, output, stage, artifacts, err := normalizeRequest(
		request.ParentPath, request.OutputName, request.StagingName, request.Artifacts,
	)
	return DirectoryRequest{ParentPath: parent, OutputName: output, StagingName: stage, Artifacts: artifacts}, err
}

func normalizeFileRequest(request FileRequest) (FileRequest, error) {
	parent, output, stage, artifacts, err := normalizeRequest(
		request.ParentPath, request.OutputName, request.StagingName, []Artifact{request.Artifact},
	)
	if err != nil {
		return FileRequest{}, err
	}
	return FileRequest{ParentPath: parent, OutputName: output, StagingName: stage, Artifact: artifacts[0]}, nil
}

func normalizeRequest(
	parentPath string,
	outputName string,
	stagingName string,
	artifacts []Artifact,
) (string, string, string, []Artifact, error) {
	if !filepath.IsAbs(parentPath) || filepath.Clean(parentPath) != parentPath {
		return "", "", "", nil, fmt.Errorf("%w: publication parent must be clean and absolute", ErrUnsafe)
	}
	if err := requireName(outputName); err != nil {
		return "", "", "", nil, err
	}
	if err := requireName(stagingName); err != nil || outputName == stagingName {
		return "", "", "", nil, fmt.Errorf("%w: staging name is invalid", ErrUnsafe)
	}
	seen := make(map[string]struct{}, len(artifacts))
	normalized := make([]Artifact, 0, len(artifacts))
	total := 0
	for _, artifact := range artifacts {
		if err := requireName(artifact.Name); err != nil {
			return "", "", "", nil, err
		}
		if _, exists := seen[artifact.Name]; exists {
			return "", "", "", nil, fmt.Errorf("%w: artifact names repeat", ErrUnsafe)
		}
		seen[artifact.Name] = struct{}{}
		if len(artifact.Bytes) < 1 || len(artifact.Bytes) > maximumArtifactBytes {
			return "", "", "", nil, fmt.Errorf("%w: artifact bytes are outside the safe range", ErrUnsafe)
		}
		total += len(artifact.Bytes)
		if total > maximumTotalBytes {
			return "", "", "", nil, fmt.Errorf("%w: artifact generation is outside the safe range", ErrUnsafe)
		}
		digest := sha256.Sum256(artifact.Bytes)
		if artifact.SHA256 != hex.EncodeToString(digest[:]) {
			return "", "", "", nil, fmt.Errorf("%w: artifact digest does not bind its bytes", ErrUnsafe)
		}
		normalized = append(normalized, Artifact{
			Name: artifact.Name, Bytes: slices.Clone(artifact.Bytes), SHA256: artifact.SHA256,
		})
	}
	return parentPath, outputName, stagingName, normalized, nil
}

func requireName(name string) error {
	if !utf8.ValidString(name) || len(name) < 1 || len(name) > maximumNameBytes ||
		name == "." || name == ".." || filepath.Base(name) != name ||
		strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("%w: publication name is not one safe path component", ErrUnsafe)
	}
	return nil
}

func classifyNamespaceError(err error) error {
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		return errors.Join(ErrCollision, err)
	}
	return unsafeError("mutate artifact publication namespace", err)
}

func unsafeError(operation string, err error) error {
	return errors.Join(ErrUnsafe, fmt.Errorf("artifact publication: %s", operation), err)
}
