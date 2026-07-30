package artifactpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

const (
	// ExistingDirectoryOutputName is deliberately singular. A successful
	// create-only install is therefore the only name that can authorize upload.
	ExistingDirectoryOutputName = "sealed"

	existingDirectoryStagingPrefix                    = ".browser-evidence-upload-"
	maximumExistingDirectoryFiles                     = 10_000
	maximumExistingDirectoryDirectories               = 20_000
	maximumExistingDirectoryFileBytes          uint64 = 512 << 20
	maximumExistingDirectoryTotalBytes         uint64 = 2 << 30
	maximumExistingDirectorySnapshotBytes      uint64 = 16 << 20
	maximumExistingDirectorySnapshotTotalBytes uint64 = 32 << 20
	maximumExistingDirectorySnapshots                 = 64
	maximumExistingDirectoryPathBytes                 = 4_096
	maximumExistingDirectoryDepth                     = 64
	maximumExistingDirectoryManifestBytes      uint64 = 8 << 20
	verificationBufferBytes                           = 64 << 10
	existingDirectoryManifestPath                     = "manifest.json"
)

// ExistingDirectoryFile binds one already-written regular file to its exact
// portable path, bigint-safe byte length, and content digest.
type ExistingDirectoryFile struct {
	RelativePath string
	ByteLength   uint64
	SHA256       string
}

// ExistingDirectoryInventory is a complete recursive namespace authority.
// Root is implicit; every other directory, including empty directories, must be
// listed explicitly so an unmentioned entry can never cross the seal boundary.
type ExistingDirectoryInventory struct {
	Directories []string
	Files       []ExistingDirectoryFile
}

// ExistingDirectoryStagingRequest creates the private, same-parent directory
// namespace before Node writes any bytes. Native creation is required on
// Windows because chmod cannot establish the hidden ACL authority used by the
// handle-relative publisher.
type ExistingDirectoryStagingRequest struct {
	ParentPath             string
	StagingName            string
	Inventory              ExistingDirectoryInventory
	ManifestPath           string
	ExpectedManifestSHA256 string
}

// ExistingDirectoryStagingReceipt is an opaque restart-revalidatable identity.
// Possessing its bytes does not grant cleanup authority; native reopening and
// exact inventory verification must still match before any removal.
type ExistingDirectoryStagingReceipt struct {
	identity outputcap.PersistentDirectoryIdentity
}

// ExistingDirectoryCleanupOutcome distinguishes a completed cleanup from a
// harmlessly absent staging name and an ambiguous namespace that was untouched.
type ExistingDirectoryCleanupOutcome string

const (
	ExistingDirectoryCleanupAbsent    ExistingDirectoryCleanupOutcome = "absent"
	ExistingDirectoryCleanupCompleted ExistingDirectoryCleanupOutcome = "completed"
	ExistingDirectoryCleanupAmbiguous ExistingDirectoryCleanupOutcome = "ambiguous"
)

type ExistingDirectoryCleanupRequest struct {
	ParentPath             string
	StagingName            string
	Inventory              ExistingDirectoryInventory
	ManifestPath           string
	ExpectedManifestSHA256 string
	Receipt                ExistingDirectoryStagingReceipt
}

// NewExistingDirectoryStagingReceipt reconstructs only the opaque comparison
// claim returned by a prior prepare invocation.
func NewExistingDirectoryStagingReceipt(encoded []byte) ExistingDirectoryStagingReceipt {
	return ExistingDirectoryStagingReceipt{identity: outputcap.NewPersistentDirectoryIdentity(encoded)}
}

func (receipt ExistingDirectoryStagingReceipt) Bytes() []byte {
	return receipt.identity.Bytes()
}

func (receipt ExistingDirectoryStagingReceipt) IsZero() bool {
	return receipt.identity.IsZero()
}

// ExistingDirectoryRequest installs an invocation-owned private sibling that
// its producer has already made quiescent. This package proves only filesystem
// authority; the in-process producer-quiescence witness belongs to the caller.
type ExistingDirectoryRequest struct {
	ParentPath             string
	OutputName             string
	StagingName            string
	Inventory              ExistingDirectoryInventory
	ManifestPath           string
	ExpectedManifestSHA256 string
	SnapshotPaths          []string
	Receipt                ExistingDirectoryStagingReceipt
}

// ExistingDirectoryVerificationRequest authenticates an already-sealed final
// directory without granting mutation authority.
type ExistingDirectoryVerificationRequest struct {
	ParentPath             string
	OutputName             string
	Inventory              ExistingDirectoryInventory
	ManifestPath           string
	ExpectedManifestSHA256 string
	SnapshotPaths          []string
}

// ExistingDirectorySnapshot contains bounded bytes reread from the exact final
// directory during the same recursive verification that authenticates it.
type ExistingDirectorySnapshot struct {
	RelativePath string
	Bytes        []byte
	SHA256       string
}

// ExistingDirectoryResult is returned only after exact final identity,
// inventory, content, and manifest authority have all been revalidated.
type ExistingDirectoryResult struct {
	ManifestSHA256 string
	Snapshots      []ExistingDirectorySnapshot
}

type normalizedExistingDirectory struct {
	parentPath             string
	outputName             string
	stagingName            string
	inventory              ExistingDirectoryInventory
	manifestPath           string
	expectedManifestSHA256 string
	snapshotPaths          []string
	tree                   *existingDirectoryNode
}

type existingDirectoryNode struct {
	relativePath string
	directories  map[string]*existingDirectoryNode
	files        map[string]ExistingDirectoryFile
}

// PublishExistingDirectory atomically installs one complete existing tree
// without replacing any final namespace entry.
func PublishExistingDirectory(request ExistingDirectoryRequest) (ExistingDirectoryResult, error) {
	return publisher{openPrivate: openPrivateNativePlatform}.publishExistingDirectory(request)
}

// PrepareExistingDirectoryStaging creates an exact empty private directory tree
// under the held publication parent. The later publisher accepts only the same
// invocation-shaped staging name and a complete file inventory.
func PrepareExistingDirectoryStaging(
	request ExistingDirectoryStagingRequest,
) (ExistingDirectoryStagingReceipt, error) {
	return publisher{openPrivate: openPrivateNativePlatform}.prepareExistingDirectoryStaging(request)
}

func CleanupExistingDirectoryStaging(
	request ExistingDirectoryCleanupRequest,
) (ExistingDirectoryCleanupOutcome, error) {
	return publisher{openPrivate: openPrivateNativePlatform}.cleanupExistingDirectoryStaging(request)
}

// VerifyExistingDirectory authenticates one previously sealed tree and returns
// only the caller-selected bounded snapshots.
func VerifyExistingDirectory(request ExistingDirectoryVerificationRequest) (ExistingDirectoryResult, error) {
	return publisher{openPrivate: openPrivateNativePlatform}.verifyExistingDirectory(request)
}

func (owner publisher) prepareExistingDirectoryStaging(
	request ExistingDirectoryStagingRequest,
) (receipt ExistingDirectoryStagingReceipt, resultErr error) {
	normalized, err := normalizeExistingDirectory(
		request.ParentPath,
		ExistingDirectoryOutputName,
		request.StagingName,
		request.Inventory,
		request.ManifestPath,
		request.ExpectedManifestSHA256,
		nil,
	)
	if err != nil {
		return ExistingDirectoryStagingReceipt{}, err
	}
	if !validExistingStagingName(normalized.stagingName) {
		return ExistingDirectoryStagingReceipt{}, fmt.Errorf("%w: existing staging name is not invocation-owned", ErrUnsafe)
	}
	// Native creation is the authority boundary for a missing publication root.
	// In particular, a Win32 mkdir cannot establish the private ACL needed by
	// handle-relative installation; an existing unsafe root is still rejected.
	platform, root, err := owner.openOrCreatePrivateRoot(normalized.parentPath)
	if err != nil {
		return ExistingDirectoryStagingReceipt{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	kind, err := root.ObserveEntry(ExistingDirectoryOutputName)
	if err != nil {
		return ExistingDirectoryStagingReceipt{}, unsafeError("observe deterministic sealed artifact destination", err)
	}
	if kind != outputcap.EntryAbsent {
		return ExistingDirectoryStagingReceipt{}, ErrCollision
	}
	stage, err := root.CreateDirectory(normalized.stagingName, true)
	if err != nil {
		return ExistingDirectoryStagingReceipt{}, classifyNamespaceError(err)
	}
	directoryHandles := map[string]outputcap.Directory{"": stage}
	fileHandles := make(map[string]outputcap.File, len(normalized.inventory.Files))
	createdDirectories := make([]string, 0, len(normalized.inventory.Directories))
	createdFiles := make([]string, 0, len(normalized.inventory.Files))
	cleanup := true
	defer func() {
		if cleanup {
			resultErr = errors.Join(resultErr, cleanupPreparedExistingDirectory(
				root, normalized.stagingName, createdDirectories, createdFiles, directoryHandles, fileHandles,
			))
		}
		for _, file := range fileHandles {
			resultErr = errors.Join(resultErr, file.Close())
		}
		for _, directory := range directoryHandles {
			resultErr = errors.Join(resultErr, directory.Close())
		}
	}()
	for _, relative := range normalized.inventory.Directories {
		parentRelative := path.Dir(relative)
		if parentRelative == "." {
			parentRelative = ""
		}
		parent := directoryHandles[parentRelative]
		child, createErr := parent.CreateDirectory(path.Base(relative), true)
		if createErr != nil {
			return ExistingDirectoryStagingReceipt{}, classifyNamespaceError(createErr)
		}
		directoryHandles[relative] = child
		createdDirectories = append(createdDirectories, relative)
	}
	for _, file := range normalized.inventory.Files {
		parentRelative := path.Dir(file.RelativePath)
		if parentRelative == "." {
			parentRelative = ""
		}
		created, createErr := directoryHandles[parentRelative].CreateFile(
			path.Base(file.RelativePath), true, int64(file.ByteLength),
		)
		if createErr != nil {
			return ExistingDirectoryStagingReceipt{}, classifyNamespaceError(createErr)
		}
		fileHandles[file.RelativePath] = created
		createdFiles = append(createdFiles, file.RelativePath)
	}
	for index := len(createdDirectories) - 1; index >= 0; index-- {
		if err := directoryHandles[createdDirectories[index]].Sync(); err != nil {
			return ExistingDirectoryStagingReceipt{}, unsafeError("sync prepared sealed artifact subdirectory", err)
		}
	}
	provider, ok := stage.(outputcap.PrivateDirectoryIdentityProvider)
	if !ok {
		return ExistingDirectoryStagingReceipt{}, unsafeError("prepare sealed artifact staging identity receipt", nil)
	}
	identity, err := provider.PreparePrivateIdentityClaim()
	if err != nil || identity.IsZero() {
		return ExistingDirectoryStagingReceipt{}, unsafeError("prepare sealed artifact staging identity receipt", err)
	}
	receipt = ExistingDirectoryStagingReceipt{identity: identity}
	if err := stage.Sync(); err != nil {
		return ExistingDirectoryStagingReceipt{}, unsafeError("sync prepared sealed artifact staging directory", err)
	}
	if err := root.Sync(); err != nil {
		return ExistingDirectoryStagingReceipt{}, unsafeError("sync prepared sealed artifact parent", err)
	}
	cleanup = false
	if owner.prepareSettlementHook != nil {
		if err := owner.prepareSettlementHook(); err != nil {
			return receipt, unsafeError("settle prepared sealed artifact handles", err)
		}
	}
	return receipt, nil
}

func cleanupPreparedExistingDirectory(
	root outputcap.Directory,
	stagingName string,
	directories []string,
	files []string,
	directoryHandles map[string]outputcap.Directory,
	fileHandles map[string]outputcap.File,
) error {
	var cleanupErr error
	for index := len(files) - 1; index >= 0; index-- {
		relative := files[index]
		parentRelative := path.Dir(relative)
		if parentRelative == "." {
			parentRelative = ""
		}
		cleanupErr = errors.Join(cleanupErr, directoryHandles[parentRelative].RemoveFile(
			path.Base(relative), fileHandles[relative],
		))
	}
	for index := len(directories) - 1; index >= 0; index-- {
		relative := directories[index]
		child := directoryHandles[relative]
		parentRelative := path.Dir(relative)
		if parentRelative == "." {
			parentRelative = ""
		}
		cleanupErr = errors.Join(cleanupErr, directoryHandles[parentRelative].RemoveDirectory(path.Base(relative), child))
	}
	cleanupErr = errors.Join(cleanupErr, root.RemoveDirectory(stagingName, directoryHandles[""]))
	if cleanupErr != nil {
		return unsafeError("clean proven-owned sealed artifact staging", cleanupErr)
	}
	return root.Sync()
}

func (owner publisher) cleanupExistingDirectoryStaging(
	request ExistingDirectoryCleanupRequest,
) (outcome ExistingDirectoryCleanupOutcome, resultErr error) {
	normalized, err := normalizeExistingDirectory(
		request.ParentPath,
		ExistingDirectoryOutputName,
		request.StagingName,
		request.Inventory,
		request.ManifestPath,
		request.ExpectedManifestSHA256,
		nil,
	)
	if err != nil || !validExistingStagingName(normalized.stagingName) || request.Receipt.IsZero() {
		return ExistingDirectoryCleanupAmbiguous, errors.Join(err, unsafeError("validate sealed artifact cleanup receipt", nil))
	}
	platform, root, err := owner.openPrivateRoot(normalized.parentPath)
	if err != nil {
		return ExistingDirectoryCleanupAmbiguous, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	kind, err := root.ObserveEntry(normalized.stagingName)
	if err != nil {
		return ExistingDirectoryCleanupAmbiguous, unsafeError("observe sealed artifact staging cleanup target", err)
	}
	if kind == outputcap.EntryAbsent {
		return ExistingDirectoryCleanupAbsent, nil
	}
	if kind != outputcap.EntryDirectory {
		return ExistingDirectoryCleanupAmbiguous, unsafeError("refuse non-directory sealed artifact cleanup target", nil)
	}
	stage, err := root.OpenDirectory(normalized.stagingName, true)
	if err != nil {
		return ExistingDirectoryCleanupAmbiguous, unsafeError("open sealed artifact staging cleanup target", err)
	}
	directoryHandles := map[string]outputcap.Directory{"": stage}
	fileHandles := make(map[string]outputcap.File, len(normalized.inventory.Files))
	defer func() {
		for _, file := range fileHandles {
			resultErr = errors.Join(resultErr, file.Close())
		}
		for _, directory := range directoryHandles {
			resultErr = errors.Join(resultErr, directory.Close())
		}
	}()
	provider, ok := stage.(outputcap.PrivateDirectoryIdentityProvider)
	if !ok {
		return ExistingDirectoryCleanupAmbiguous, unsafeError("revalidate sealed artifact staging cleanup receipt", nil)
	}
	identity, err := provider.PrivateIdentityClaim()
	if err != nil || !identity.Equal(request.Receipt.identity) {
		return ExistingDirectoryCleanupAmbiguous, unsafeError("sealed artifact staging cleanup receipt does not match", err)
	}
	if err := openPreparedExistingNode(stage, normalized.tree, directoryHandles, fileHandles); err != nil {
		return ExistingDirectoryCleanupAmbiguous, err
	}
	files := make([]string, 0, len(normalized.inventory.Files))
	for _, file := range normalized.inventory.Files {
		files = append(files, file.RelativePath)
	}
	if err := cleanupPreparedExistingDirectory(
		root,
		normalized.stagingName,
		normalized.inventory.Directories,
		files,
		directoryHandles,
		fileHandles,
	); err != nil {
		return ExistingDirectoryCleanupAmbiguous, err
	}
	return ExistingDirectoryCleanupCompleted, nil
}

func openPreparedExistingNode(
	directory outputcap.Directory,
	node *existingDirectoryNode,
	directoryHandles map[string]outputcap.Directory,
	fileHandles map[string]outputcap.File,
) error {
	expectedNames := make([]string, 0, len(node.directories)+len(node.files))
	for name := range node.directories {
		expectedNames = append(expectedNames, name)
	}
	for name := range node.files {
		expectedNames = append(expectedNames, name)
	}
	sort.Strings(expectedNames)
	actualNames, err := directory.Names(len(expectedNames) + 1)
	if err != nil {
		return unsafeError("enumerate exact sealed artifact cleanup namespace", err)
	}
	sort.Strings(actualNames)
	if !slices.Equal(expectedNames, actualNames) {
		return unsafeError("refuse sealed artifact cleanup with unexpected namespace entries", nil)
	}
	for _, name := range expectedNames {
		if child, ok := node.directories[name]; ok {
			opened, err := directory.OpenDirectory(name, true)
			if err != nil {
				return unsafeError("open sealed artifact cleanup subdirectory", err)
			}
			directoryHandles[child.relativePath] = opened
			if err := openPreparedExistingNode(opened, child, directoryHandles, fileHandles); err != nil {
				return err
			}
			continue
		}
		expected := node.files[name]
		opened, err := directory.OpenFile(name, true, true)
		if err != nil {
			return unsafeError("open sealed artifact cleanup file", err)
		}
		fileHandles[expected.RelativePath] = opened
		size, err := opened.Size()
		if err != nil || size != expected.ByteLength {
			return unsafeError("refuse sealed artifact cleanup after file-size change", err)
		}
	}
	return nil
}

func (owner publisher) publishExistingDirectory(
	request ExistingDirectoryRequest,
) (result ExistingDirectoryResult, resultErr error) {
	normalized, err := normalizeExistingDirectoryRequest(request)
	if err != nil {
		return ExistingDirectoryResult{}, err
	}
	platform, root, err := owner.openPrivateRoot(normalized.parentPath)
	if err != nil {
		return ExistingDirectoryResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	stage, err := root.OpenDirectory(normalized.stagingName, true)
	if err != nil {
		return ExistingDirectoryResult{}, unsafeError("open invocation-owned artifact staging directory", err)
	}
	defer func() { resultErr = errors.Join(resultErr, stage.Close()) }()
	if err := verifyExistingDirectoryReceipt(stage, request.Receipt); err != nil {
		return ExistingDirectoryResult{}, err
	}
	state := &transactionState{
		root: root, stage: stage, stageName: normalized.stagingName, outputName: normalized.outputName,
	}
	if err := verifyNamedExistingDirectory(
		root, stage, normalized.stagingName, normalized, request.Receipt, false,
	); err != nil {
		return ExistingDirectoryResult{}, err
	}
	if err := owner.cross(boundaryBeforeCommit, state); err != nil {
		return ExistingDirectoryResult{}, err
	}
	// Repeating the complete streaming verification after the hostile boundary
	// proves the quiescent tree did not change before its native commit cut.
	if err := verifyNamedExistingDirectory(
		root, stage, normalized.stagingName, normalized, request.Receipt, true,
	); err != nil {
		return ExistingDirectoryResult{}, err
	}
	if err := owner.cross(boundaryBeforeNativeCommit, state); err != nil {
		return ExistingDirectoryResult{}, err
	}
	installed, err := root.InstallDirectoryNoReplace(stage, normalized.outputName)
	if err != nil {
		return ExistingDirectoryResult{}, classifyNamespaceError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, installed.Close()) }()
	state.installed = installed
	if err := owner.cross(boundaryAfterCommit, state); err != nil {
		return ExistingDirectoryResult{}, err
	}
	if _, err := verifyExistingDirectoryTree(installed, normalized, true, nil); err != nil {
		return ExistingDirectoryResult{}, err
	}
	if err := installed.Sync(); err != nil {
		return ExistingDirectoryResult{}, unsafeError("sync sealed artifact directory", err)
	}
	if err := root.Sync(); err != nil {
		return ExistingDirectoryResult{}, unsafeError("sync sealed artifact parent", err)
	}
	if err := owner.cross(boundaryAfterDurability, state); err != nil {
		return ExistingDirectoryResult{}, err
	}
	return owner.reopenExistingDirectoryResult(normalized.parentPath, root, installed, normalized)
}

func (owner publisher) verifyExistingDirectory(
	request ExistingDirectoryVerificationRequest,
) (result ExistingDirectoryResult, resultErr error) {
	normalized, err := normalizeExistingDirectoryVerificationRequest(request)
	if err != nil {
		return ExistingDirectoryResult{}, err
	}
	platform, root, err := owner.openPrivateRoot(normalized.parentPath)
	if err != nil {
		return ExistingDirectoryResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	final, err := root.OpenDirectory(normalized.outputName, true)
	if err != nil {
		return ExistingDirectoryResult{}, unsafeError("open sealed artifact directory", err)
	}
	defer func() { resultErr = errors.Join(resultErr, final.Close()) }()
	return verifyExistingDirectoryTree(final, normalized, false, normalized.snapshotPaths)
}

func (owner publisher) reopenExistingDirectoryResult(
	parentPath string,
	originalRoot outputcap.Directory,
	installed outputcap.Directory,
	normalized normalizedExistingDirectory,
) (result ExistingDirectoryResult, resultErr error) {
	platform, root, err := owner.openPrivateRoot(parentPath)
	if err != nil {
		return ExistingDirectoryResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	sameRoot, err := originalRoot.SameDirectory(root)
	if err != nil || !sameRoot {
		return ExistingDirectoryResult{}, unsafeError("reopen exact sealed artifact parent", err)
	}
	final, err := root.OpenDirectory(normalized.outputName, true)
	if err != nil {
		return ExistingDirectoryResult{}, unsafeError("reopen sealed artifact directory", err)
	}
	defer func() { resultErr = errors.Join(resultErr, final.Close()) }()
	sameFinal, err := installed.SameDirectory(final)
	if err != nil || !sameFinal {
		return ExistingDirectoryResult{}, unsafeError("verify sealed artifact directory identity", err)
	}
	return verifyExistingDirectoryTree(final, normalized, false, normalized.snapshotPaths)
}

func verifyNamedExistingDirectory(
	root outputcap.Directory,
	stage outputcap.Directory,
	stageName string,
	normalized normalizedExistingDirectory,
	receipt ExistingDirectoryStagingReceipt,
	syncFiles bool,
) error {
	named, err := root.OpenDirectory(stageName, true)
	if err != nil {
		return unsafeError("reopen artifact staging directory", err)
	}
	defer func() { _ = named.Close() }()
	same, err := stage.SameDirectory(named)
	if err != nil || !same {
		return unsafeError("verify artifact staging directory identity", err)
	}
	// SameDirectory closes namespace races; the persistent receipt additionally
	// proves this is the private object created by the authorized prepare call.
	if err := verifyExistingDirectoryReceipt(named, receipt); err != nil {
		return err
	}
	_, err = verifyExistingDirectoryTree(named, normalized, syncFiles, nil)
	return err
}

func verifyExistingDirectoryReceipt(
	directory outputcap.Directory,
	receipt ExistingDirectoryStagingReceipt,
) error {
	if receipt.IsZero() {
		return unsafeError("validate sealed artifact staging receipt", nil)
	}
	provider, ok := directory.(outputcap.PrivateDirectoryIdentityProvider)
	if !ok {
		return unsafeError("revalidate sealed artifact staging receipt", nil)
	}
	identity, err := provider.PrivateIdentityClaim()
	if err != nil || !identity.Equal(receipt.identity) {
		return unsafeError("sealed artifact staging receipt does not match", err)
	}
	return nil
}

func verifyExistingDirectoryTree(
	root outputcap.Directory,
	normalized normalizedExistingDirectory,
	syncFiles bool,
	snapshotPaths []string,
) (ExistingDirectoryResult, error) {
	snapshotSet := make(map[string]struct{}, len(snapshotPaths))
	for _, snapshotPath := range snapshotPaths {
		snapshotSet[snapshotPath] = struct{}{}
	}
	snapshots := make(map[string]ExistingDirectorySnapshot, len(snapshotPaths))
	if err := verifyExistingNode(root, normalized.tree, syncFiles, snapshotSet, snapshots); err != nil {
		return ExistingDirectoryResult{}, err
	}
	manifest := findExistingFile(normalized.inventory.Files, normalized.manifestPath)
	if manifest == nil {
		return ExistingDirectoryResult{}, unsafeError("locate sealed artifact manifest authority", nil)
	}
	orderedSnapshots := make([]ExistingDirectorySnapshot, 0, len(snapshotPaths))
	for _, snapshotPath := range snapshotPaths {
		snapshot, ok := snapshots[snapshotPath]
		if !ok {
			return ExistingDirectoryResult{}, unsafeError("return every requested sealed artifact snapshot", nil)
		}
		orderedSnapshots = append(orderedSnapshots, snapshot)
	}
	return ExistingDirectoryResult{ManifestSHA256: manifest.SHA256, Snapshots: orderedSnapshots}, nil
}

func verifyExistingNode(
	directory outputcap.Directory,
	node *existingDirectoryNode,
	syncFiles bool,
	snapshotPaths map[string]struct{},
	snapshots map[string]ExistingDirectorySnapshot,
) (resultErr error) {
	expectedNames := make([]string, 0, len(node.directories)+len(node.files))
	for name := range node.directories {
		expectedNames = append(expectedNames, name)
	}
	for name := range node.files {
		expectedNames = append(expectedNames, name)
	}
	sort.Strings(expectedNames)
	actualNames, err := directory.Names(len(expectedNames) + 1)
	if err != nil {
		return unsafeError("enumerate exact sealed artifact directory", err)
	}
	sort.Strings(actualNames)
	if !slices.Equal(actualNames, expectedNames) {
		return unsafeError("verify exact sealed artifact directory entries", nil)
	}
	for _, name := range expectedNames {
		if child, ok := node.directories[name]; ok {
			opened, err := directory.OpenDirectory(name, true)
			if err != nil {
				return unsafeError("open sealed artifact subdirectory", err)
			}
			childErr := verifyExistingNode(opened, child, syncFiles, snapshotPaths, snapshots)
			if childErr == nil && syncFiles {
				childErr = opened.Sync()
			}
			closeErr := opened.Close()
			if childErr != nil || closeErr != nil {
				return errors.Join(childErr, unsafeError("close verified sealed artifact subdirectory", closeErr))
			}
			continue
		}
		expected := node.files[name]
		opened, err := directory.OpenFile(name, true, syncFiles)
		if err != nil {
			return unsafeError("open sealed artifact file", err)
		}
		_, snapshotRequested := snapshotPaths[expected.RelativePath]
		snapshot, readErr := verifyExistingFile(opened, expected, snapshotRequested)
		if readErr == nil && syncFiles {
			readErr = opened.Sync()
		}
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(readErr, unsafeError("close verified sealed artifact file", closeErr))
		}
		if snapshotRequested {
			snapshots[expected.RelativePath] = snapshot
		}
	}
	return nil
}

func verifyExistingFile(
	file outputcap.File,
	expected ExistingDirectoryFile,
	snapshotRequested bool,
) (ExistingDirectorySnapshot, error) {
	before, err := file.Size()
	if err != nil || before != expected.ByteLength {
		return ExistingDirectorySnapshot{}, unsafeError("verify sealed artifact file size", err)
	}
	digest := sha256.New()
	var snapshot []byte
	if snapshotRequested {
		snapshot = make([]byte, int(expected.ByteLength))
	}
	buffer := make([]byte, verificationBufferBytes)
	var offset uint64
	for offset < expected.ByteLength {
		remaining := expected.ByteLength - offset
		chunk := buffer
		if remaining < uint64(len(chunk)) {
			chunk = chunk[:int(remaining)]
		}
		read, readErr := file.ReadAt(chunk, int64(offset))
		if read > 0 {
			_, _ = digest.Write(chunk[:read])
			if snapshotRequested {
				copy(snapshot[int(offset):], chunk[:read])
			}
			offset += uint64(read)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return ExistingDirectorySnapshot{}, unsafeError("stream sealed artifact file", readErr)
		}
		if read == 0 {
			return ExistingDirectorySnapshot{}, unsafeError("stream exact sealed artifact bytes", io.ErrUnexpectedEOF)
		}
	}
	after, err := file.Size()
	if err != nil || after != before {
		return ExistingDirectorySnapshot{}, unsafeError("revalidate sealed artifact file size", err)
	}
	encodedDigest := hex.EncodeToString(digest.Sum(nil))
	if encodedDigest != expected.SHA256 {
		return ExistingDirectorySnapshot{}, unsafeError("verify sealed artifact file digest", nil)
	}
	return ExistingDirectorySnapshot{
		RelativePath: expected.RelativePath,
		Bytes:        snapshot,
		SHA256:       encodedDigest,
	}, nil
}

func normalizeExistingDirectoryRequest(request ExistingDirectoryRequest) (normalizedExistingDirectory, error) {
	if !validExistingStagingName(request.StagingName) {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: existing staging name is not invocation-owned", ErrUnsafe)
	}
	if request.Receipt.IsZero() {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: existing staging receipt is required", ErrUnsafe)
	}
	return normalizeExistingDirectory(
		request.ParentPath,
		request.OutputName,
		request.StagingName,
		request.Inventory,
		request.ManifestPath,
		request.ExpectedManifestSHA256,
		request.SnapshotPaths,
	)
}

func normalizeExistingDirectoryVerificationRequest(
	request ExistingDirectoryVerificationRequest,
) (normalizedExistingDirectory, error) {
	return normalizeExistingDirectory(
		request.ParentPath,
		request.OutputName,
		"",
		request.Inventory,
		request.ManifestPath,
		request.ExpectedManifestSHA256,
		request.SnapshotPaths,
	)
}

func normalizeExistingDirectory(
	parentPath string,
	outputName string,
	stagingName string,
	inventory ExistingDirectoryInventory,
	manifestPath string,
	expectedManifestSHA256 string,
	snapshotPaths []string,
) (normalizedExistingDirectory, error) {
	if !filepath.IsAbs(parentPath) || filepath.Clean(parentPath) != parentPath {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact parent must be clean and absolute", ErrUnsafe)
	}
	if outputName != ExistingDirectoryOutputName {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact output name is not deterministic", ErrUnsafe)
	}
	if manifestPath != existingDirectoryManifestPath {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact manifest path is not canonical", ErrUnsafe)
	}
	if !isSHA256(expectedManifestSHA256) {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: expected sealed artifact manifest digest is invalid", ErrUnsafe)
	}
	if len(inventory.Files) < 1 || len(inventory.Files) > maximumExistingDirectoryFiles ||
		len(inventory.Directories) > maximumExistingDirectoryDirectories {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact inventory exceeds its entry authority", ErrUnsafe)
	}
	directories := slices.Clone(inventory.Directories)
	files := slices.Clone(inventory.Files)
	if !sort.StringsAreSorted(directories) || !slices.IsSortedFunc(files, compareExistingFiles) {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact inventory is not canonically ordered", ErrUnsafe)
	}
	directorySet := make(map[string]struct{}, len(directories))
	portableSet := make(map[string]struct{}, len(directories)+len(files))
	for _, directory := range directories {
		if err := requirePortableExistingPath(directory); err != nil {
			return normalizedExistingDirectory{}, err
		}
		if _, repeated := directorySet[directory]; repeated {
			return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact directory repeats", ErrUnsafe)
		}
		key := portableExistingPathKey(directory)
		if _, collided := portableSet[key]; collided {
			return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact paths collide portably", ErrUnsafe)
		}
		directorySet[directory] = struct{}{}
		portableSet[key] = struct{}{}
	}
	fileSet := make(map[string]ExistingDirectoryFile, len(files))
	var totalBytes uint64
	for _, file := range files {
		if err := requirePortableExistingPath(file.RelativePath); err != nil {
			return normalizedExistingDirectory{}, err
		}
		if file.ByteLength > maximumExistingDirectoryFileBytes || !isSHA256(file.SHA256) {
			return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact file metadata is outside its authority", ErrUnsafe)
		}
		if totalBytes > maximumExistingDirectoryTotalBytes-file.ByteLength {
			return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact bytes exceed their total authority", ErrUnsafe)
		}
		totalBytes += file.ByteLength
		if _, repeated := fileSet[file.RelativePath]; repeated {
			return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact file repeats", ErrUnsafe)
		}
		key := portableExistingPathKey(file.RelativePath)
		if _, collided := portableSet[key]; collided {
			return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact paths collide portably", ErrUnsafe)
		}
		fileSet[file.RelativePath] = file
		portableSet[key] = struct{}{}
	}
	manifestFile, ok := fileSet[manifestPath]
	if !ok {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact manifest is absent from inventory", ErrUnsafe)
	}
	if manifestFile.ByteLength < 1 || manifestFile.ByteLength > maximumExistingDirectoryManifestBytes ||
		manifestFile.SHA256 != expectedManifestSHA256 {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact manifest does not match external authority", ErrUnsafe)
	}
	for _, directory := range directories {
		parent := path.Dir(directory)
		if parent != "." {
			if _, ok := directorySet[parent]; !ok {
				return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact directory parent is absent", ErrUnsafe)
			}
		}
	}
	for _, file := range files {
		parent := path.Dir(file.RelativePath)
		if parent != "." {
			if _, ok := directorySet[parent]; !ok {
				return normalizedExistingDirectory{}, fmt.Errorf("%w: sealed artifact file parent is absent", ErrUnsafe)
			}
		}
	}
	normalizedSnapshots, err := normalizeExistingSnapshots(snapshotPaths, fileSet)
	if err != nil {
		return normalizedExistingDirectory{}, err
	}
	tree := buildExistingDirectoryTree(directories, files)
	return normalizedExistingDirectory{
		parentPath:             parentPath,
		outputName:             outputName,
		stagingName:            stagingName,
		inventory:              ExistingDirectoryInventory{Directories: directories, Files: files},
		manifestPath:           manifestPath,
		expectedManifestSHA256: expectedManifestSHA256,
		snapshotPaths:          normalizedSnapshots,
		tree:                   tree,
	}, nil
}

func normalizeExistingSnapshots(
	snapshotPaths []string,
	files map[string]ExistingDirectoryFile,
) ([]string, error) {
	if len(snapshotPaths) > maximumExistingDirectorySnapshots || !sort.StringsAreSorted(snapshotPaths) {
		return nil, fmt.Errorf("%w: sealed artifact snapshots are not canonically bounded", ErrUnsafe)
	}
	normalized := slices.Clone(snapshotPaths)
	var total uint64
	for index, snapshotPath := range normalized {
		if index > 0 && normalized[index-1] == snapshotPath {
			return nil, fmt.Errorf("%w: sealed artifact snapshot repeats", ErrUnsafe)
		}
		file, ok := files[snapshotPath]
		if !ok || file.ByteLength > maximumExistingDirectorySnapshotBytes ||
			total > maximumExistingDirectorySnapshotTotalBytes-file.ByteLength {
			return nil, fmt.Errorf("%w: sealed artifact snapshots exceed their byte authority", ErrUnsafe)
		}
		total += file.ByteLength
	}
	return normalized, nil
}

func buildExistingDirectoryTree(
	directories []string,
	files []ExistingDirectoryFile,
) *existingDirectoryNode {
	root := &existingDirectoryNode{directories: map[string]*existingDirectoryNode{}, files: map[string]ExistingDirectoryFile{}}
	nodes := map[string]*existingDirectoryNode{"": root}
	for _, relative := range directories {
		parentPath := path.Dir(relative)
		if parentPath == "." {
			parentPath = ""
		}
		parent := nodes[parentPath]
		node := &existingDirectoryNode{
			relativePath: relative,
			directories:  map[string]*existingDirectoryNode{},
			files:        map[string]ExistingDirectoryFile{},
		}
		parent.directories[path.Base(relative)] = node
		nodes[relative] = node
	}
	for _, file := range files {
		parentPath := path.Dir(file.RelativePath)
		if parentPath == "." {
			parentPath = ""
		}
		nodes[parentPath].files[path.Base(file.RelativePath)] = file
	}
	return root
}

func requirePortableExistingPath(value string) error {
	if !utf8.ValidString(value) || !norm.NFC.IsNormalString(value) || len(value) < 1 ||
		len(value) > maximumExistingDirectoryPathBytes || strings.HasPrefix(value, "/") ||
		strings.ContainsAny(value, "\\:<>\"|?*\x00") {
		return fmt.Errorf("%w: sealed artifact path is not portable NFC", ErrUnsafe)
	}
	segments := strings.Split(value, "/")
	if len(segments) > maximumExistingDirectoryDepth {
		return fmt.Errorf("%w: sealed artifact path exceeds its depth authority", ErrUnsafe)
	}
	for _, segment := range segments {
		if len(segment) < 1 || len(segment) > maximumNameBytes || segment == "." || segment == ".." ||
			strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") ||
			containsPortableControl(segment) || containsNonASCIIExistingCase(segment) || isDOSDeviceName(segment) {
			return fmt.Errorf("%w: sealed artifact path contains a non-portable component", ErrUnsafe)
		}
	}
	return nil
}

func containsPortableControl(value string) bool {
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return true
		}
	}
	return false
}

func isDOSDeviceName(segment string) bool {
	base := strings.ToUpper(strings.SplitN(segment, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CLOCK$" ||
		base == "CONIN$" || base == "CONOUT$" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		suffix := strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT")
		return suffix == "¹" || suffix == "²" || suffix == "³"
	}
	return false
}

func portableExistingPathKey(value string) string {
	return strings.Map(func(current rune) rune {
		if current >= 'A' && current <= 'Z' {
			return current + ('a' - 'A')
		}
		return current
	}, value)
}

func containsNonASCIIExistingCase(value string) bool {
	for _, current := range value {
		if current <= 0x7f {
			continue
		}
		scalar := string(current)
		if cases.Upper(language.Und).String(scalar) != cases.Lower(language.Und).String(scalar) {
			return true
		}
	}
	return false
}

func validExistingStagingName(value string) bool {
	if !strings.HasPrefix(value, existingDirectoryStagingPrefix) ||
		len(value) != len(existingDirectoryStagingPrefix)+32 {
		return false
	}
	for _, current := range value[len(existingDirectoryStagingPrefix):] {
		if (current < '0' || current > '9') && (current < 'a' || current > 'f') {
			return false
		}
	}
	return true
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func compareExistingFiles(left, right ExistingDirectoryFile) int {
	return strings.Compare(left.RelativePath, right.RelativePath)
}

func findExistingFile(files []ExistingDirectoryFile, relativePath string) *ExistingDirectoryFile {
	index, found := slices.BinarySearchFunc(files, ExistingDirectoryFile{RelativePath: relativePath}, compareExistingFiles)
	if !found {
		return nil
	}
	return &files[index]
}
