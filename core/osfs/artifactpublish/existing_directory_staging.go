package artifactpublish

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (owner publisher) prepareExistingDirectoryStaging(
	request ExistingDirectoryStagingRequest,
) (receipt ExistingDirectoryStagingReceipt, resultErr error) {
	normalized, err := normalizeExistingDirectoryStagingRequest(request)
	if err != nil {
		return ExistingDirectoryStagingReceipt{}, err
	}
	// Native creation is the authority boundary for a missing publication root.
	// In particular, a Win32 mkdir cannot establish the private ACL needed by
	// handle-relative installation; an existing unsafe root is still rejected.
	platform, root, err := owner.openOrCreatePrivateRoot(normalized.parentPath)
	if err != nil {
		return ExistingDirectoryStagingReceipt{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()

	stage, err := createExistingDirectoryStaging(root, normalized)
	if err != nil {
		return ExistingDirectoryStagingReceipt{}, err
	}
	preparation := newExistingDirectoryStagingPreparation(root, stage, normalized)
	defer func() {
		resultErr = preparation.joinReleaseErrors(resultErr)
	}()

	if err := preparation.createInventory(normalized.inventory); err != nil {
		return ExistingDirectoryStagingReceipt{}, err
	}
	receipt, err = preparation.prepareDurableReceipt()
	if err != nil {
		return ExistingDirectoryStagingReceipt{}, err
	}
	if err := owner.settleExistingDirectoryPreparation(); err != nil {
		return receipt, err
	}
	return receipt, nil
}

type existingDirectoryStagingPreparation struct {
	root               outputcap.Directory
	stage              outputcap.Directory
	stagingName        string
	directoryHandles   map[string]outputcap.Directory
	fileHandles        map[string]outputcap.File
	createdDirectories []string
	createdFiles       []string
	cleanupRequired    bool
}

func normalizeExistingDirectoryStagingRequest(
	request ExistingDirectoryStagingRequest,
) (normalizedExistingDirectory, error) {
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
		return normalizedExistingDirectory{}, err
	}
	if !validExistingStagingName(normalized.stagingName) {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: existing staging name is not invocation-owned", ErrUnsafe)
	}
	return normalized, nil
}

func createExistingDirectoryStaging(
	root outputcap.Directory,
	normalized normalizedExistingDirectory,
) (outputcap.Directory, error) {
	kind, err := root.ObserveEntry(normalized.outputName)
	if err != nil {
		return nil, unsafeError("observe deterministic sealed artifact destination", err)
	}
	if kind != outputcap.EntryAbsent {
		return nil, ErrCollision
	}
	stage, err := root.CreateDirectory(normalized.stagingName, true)
	if err != nil {
		return nil, classifyNamespaceError(err)
	}
	return stage, nil
}

func newExistingDirectoryStagingPreparation(
	root outputcap.Directory,
	stage outputcap.Directory,
	normalized normalizedExistingDirectory,
) *existingDirectoryStagingPreparation {
	return &existingDirectoryStagingPreparation{
		root:               root,
		stage:              stage,
		stagingName:        normalized.stagingName,
		directoryHandles:   map[string]outputcap.Directory{"": stage},
		fileHandles:        make(map[string]outputcap.File, len(normalized.inventory.Files)),
		createdDirectories: make([]string, 0, len(normalized.inventory.Directories)),
		createdFiles:       make([]string, 0, len(normalized.inventory.Files)),
		cleanupRequired:    true,
	}
}

func (preparation *existingDirectoryStagingPreparation) createInventory(
	inventory ExistingDirectoryInventory,
) error {
	if err := preparation.createDirectories(inventory.Directories); err != nil {
		return err
	}
	return preparation.createFiles(inventory.Files)
}

func (preparation *existingDirectoryStagingPreparation) createDirectories(directories []string) error {
	for _, relative := range directories {
		parent := preparation.directoryHandles[parentDirectoryPath(relative)]
		child, err := parent.CreateDirectory(path.Base(relative), true)
		if err != nil {
			return classifyNamespaceError(err)
		}
		preparation.directoryHandles[relative] = child
		preparation.createdDirectories = append(preparation.createdDirectories, relative)
	}
	return nil
}

func (preparation *existingDirectoryStagingPreparation) createFiles(files []ExistingDirectoryFile) error {
	for _, file := range files {
		parent := preparation.directoryHandles[parentDirectoryPath(file.RelativePath)]
		created, err := parent.CreateFile(path.Base(file.RelativePath), true, int64(file.ByteLength))
		if err != nil {
			return classifyNamespaceError(err)
		}
		preparation.fileHandles[file.RelativePath] = created
		preparation.createdFiles = append(preparation.createdFiles, file.RelativePath)
	}
	return nil
}

func parentDirectoryPath(relative string) string {
	parentRelative := path.Dir(relative)
	if parentRelative == "." {
		return ""
	}
	return parentRelative
}

func (preparation *existingDirectoryStagingPreparation) prepareDurableReceipt() (ExistingDirectoryStagingReceipt, error) {
	for index := len(preparation.createdDirectories) - 1; index >= 0; index-- {
		directory := preparation.directoryHandles[preparation.createdDirectories[index]]
		if err := directory.Sync(); err != nil {
			return ExistingDirectoryStagingReceipt{}, unsafeError("sync prepared sealed artifact subdirectory", err)
		}
	}
	provider, ok := preparation.stage.(outputcap.PrivateDirectoryIdentityProvider)
	if !ok {
		return ExistingDirectoryStagingReceipt{}, unsafeError("prepare sealed artifact staging identity receipt", nil)
	}
	identity, err := provider.PreparePrivateIdentityClaim()
	if err != nil || identity.IsZero() {
		return ExistingDirectoryStagingReceipt{}, unsafeError("prepare sealed artifact staging identity receipt", err)
	}
	if err := preparation.stage.Sync(); err != nil {
		return ExistingDirectoryStagingReceipt{}, unsafeError("sync prepared sealed artifact staging directory", err)
	}
	if err := preparation.root.Sync(); err != nil {
		return ExistingDirectoryStagingReceipt{}, unsafeError("sync prepared sealed artifact parent", err)
	}
	// Once the parent is durable, deleting the stage on a later Close ambiguity
	// would destroy the only authority the returned receipt can safely recover.
	preparation.cleanupRequired = false
	return ExistingDirectoryStagingReceipt{identity: identity}, nil
}

func (preparation *existingDirectoryStagingPreparation) joinReleaseErrors(resultErr error) error {
	if preparation.cleanupRequired {
		resultErr = errors.Join(resultErr, cleanupPreparedExistingDirectory(
			preparation.root,
			preparation.stagingName,
			preparation.createdDirectories,
			preparation.createdFiles,
			preparation.directoryHandles,
			preparation.fileHandles,
		))
	}
	for _, file := range preparation.fileHandles {
		resultErr = errors.Join(resultErr, file.Close())
	}
	for _, directory := range preparation.directoryHandles {
		resultErr = errors.Join(resultErr, directory.Close())
	}
	return resultErr
}

func (owner publisher) settleExistingDirectoryPreparation() error {
	if owner.prepareSettlementHook == nil {
		return nil
	}
	if err := owner.prepareSettlementHook(); err != nil {
		return unsafeError("settle prepared sealed artifact handles", err)
	}
	return nil
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
