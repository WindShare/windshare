package artifactpublish

import (
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

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
