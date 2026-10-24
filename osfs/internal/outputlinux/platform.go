//go:build linux

package outputlinux

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type linuxPlatform struct {
	rootOpenDisposition outputcap.RootOpenDisposition
	root                *linuxDirectory
}

type linuxDirectoryOrigin struct {
	parent *linuxOutputDirectory
	name   string
}

type linuxDirectory struct {
	native *linuxOutputDirectory
	origin *linuxDirectoryOrigin
}

type linuxFileOrigin struct {
	parent *linuxOutputDirectory
	name   string
}

type linuxFileState struct {
	native   *linuxOutputRegularFile
	origin   *linuxFileOrigin
	private  bool
	borrowed bool
}

type linuxObservedFile struct {
	state *linuxFileState
}

type linuxRecoveryDurabilityFile struct {
	state *linuxFileState
}

type linuxMutableFile struct {
	state *linuxFileState
}

type linuxLock struct {
	native *linuxOutputStableLock
	file   *linuxMutableFile
}

type linuxEntryRef struct {
	native *linuxOutputPinnedEntry
}

func Open(path string, create bool) (outputcap.Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			linuxUnsafe("open output root", "output root must be absolute", nil))
	}
	clean := filepath.Clean(path)
	rootOpenDisposition := outputcap.CallerProvidedContainer
	root, err := linuxOpenOutputRoot(clean, &linuxHostOutputSystem)
	if create && errors.Is(err, fs.ErrNotExist) {
		root, err = linuxCreateOutputRoot(clean)
		if err == nil {
			rootOpenDisposition = outputcap.AuthorityCreatedRoot
		}
	}
	if err != nil {
		return nil, linuxError(err)
	}
	return &linuxPlatform{
		rootOpenDisposition: rootOpenDisposition,
		root:                &linuxDirectory{native: root},
	}, nil
}

func linuxCreateOutputRoot(path string) (_ *linuxOutputDirectory, resultErr error) {
	const operation = "create output root"
	candidate := path
	missing := make([]string, 0, 4)
	var current *linuxOutputDirectory
	for {
		opened, err := linuxOpenOutputRoot(candidate, &linuxHostOutputSystem)
		if err == nil {
			current = opened
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return nil, errors.Join(linuxUnsafe(operation,
				"no existing accessible ancestor contains the requested root", nil), err)
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
	defer func() {
		if current != nil {
			resultErr = errors.Join(resultErr, current.close())
		}
	}()
	for index := range slices.Backward(missing) {
		created, err := current.createDirectoryExact(missing[index], linuxPublicDirectoryCreateMode)
		if err != nil {
			return nil, err
		}
		if err := current.close(); err != nil {
			return nil, errors.Join(err, created.close())
		}
		current = created
	}

	// Reopening the full path proves that the requested spelling still resolves
	// to the handle-created object; a concurrent rename cannot redirect the new
	// root between safe creation and authority return.
	reopened, err := linuxOpenOutputRoot(path, &linuxHostOutputSystem)
	if err != nil {
		return nil, err
	}
	same, compareErr := linuxSameOpenDirectory(current, reopened)
	if compareErr != nil || !same {
		return nil, errors.Join(linuxUnsafe(operation,
			"reopened root differs from the handle-created directory", nil), compareErr, reopened.close())
	}
	if err := current.close(); err != nil {
		return nil, errors.Join(err, reopened.close())
	}
	current = nil
	return reopened, nil
}

func (platform *linuxPlatform) Root() outputcap.Directory {
	if platform == nil {
		return nil
	}
	return platform.root
}

func (platform *linuxPlatform) RootOpenDisposition() outputcap.RootOpenDisposition {
	if platform == nil {
		return ""
	}
	return platform.rootOpenDisposition
}

// linuxOutputPublicOperationGuard owns a fresh pinned root capability for one
// operation. Revalidating effective access before duplication keeps admission
// from becoming a stale authorization decision, while the duplicate prevents
// callers from closing or retaining the platform's lifetime root.
type linuxOutputPublicOperationGuard struct {
	root outputcap.Directory
}

func (guard *linuxOutputPublicOperationGuard) Root() outputcap.Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

func (guard *linuxOutputPublicOperationGuard) Close() error {
	if guard == nil || guard.root == nil {
		return nil
	}
	err := guard.root.Close()
	guard.root = nil
	return err
}

func (platform *linuxPlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux output platform is closed"))
	}
	if err := platform.root.native.validatePublicCreateAuthority(); err != nil {
		return nil, linuxError(err)
	}
	root, err := platform.root.Duplicate()
	if err != nil {
		return nil, err
	}
	return &linuxOutputPublicOperationGuard{root: root}, nil
}

func (platform *linuxPlatform) Certification() outputcap.CertificationID {
	if platform == nil || platform.root == nil || platform.root.native == nil ||
		platform.root.native.binding.restart == nil {
		return ""
	}
	return outputcap.CertificationLinuxExt4ProcessRestart
}

func (platform *linuxPlatform) RootBinding() (outputcap.OutputRootBinding, error) {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux output platform is closed"),
		)
	}
	root := platform.root.native
	if err := root.verifyHandle(); err != nil {
		return outputcap.OutputRootBinding{}, linuxError(err)
	}
	binding := root.binding
	if binding.restart == nil {
		return outputcap.OutputRootBinding{}, outputcap.ErrRecoverableOutputUnsupported
	}
	volume, err := linuxEncodeMountIdentity(binding.mount)
	if err != nil {
		return outputcap.OutputRootBinding{}, linuxError(err)
	}
	if root.system.restartIdentity == nil {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrRecoverableOutputUnsupported,
			errors.New("osfs: Linux directory restart-identity provider is unavailable"),
		)
	}
	restartIdentity, err := root.system.restartIdentity.Read(root.system, root.fd, binding.mount)
	if err != nil {
		return outputcap.OutputRootBinding{}, linuxError(err)
	}
	if !restartIdentity.matchesHandle(root.object) ||
		!restartIdentity.sameDirectory(binding.restart.rootIdentity) {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux output-root restart identity changed"),
		)
	}
	object, err := linuxEncodeDirectoryRestartIdentity(restartIdentity)
	if err != nil {
		return outputcap.OutputRootBinding{}, linuxError(err)
	}
	rootBinding, err := outputcap.NewOutputRootBinding(platform.Certification(), volume, object)
	return rootBinding, linuxError(err)
}

func (platform *linuxPlatform) Durability() transfer.DurabilityLevel {
	if platform.Certification() == "" {
		return transfer.DurabilityNone
	}
	return transfer.DurabilityProcessRestart
}

func (platform *linuxPlatform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	if platform.Certification() == "" {
		return 0
	}
	return checkpointmodel.LiveCleanupLinuxExt4V1
}

func (platform *linuxPlatform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return outputcap.DestinationCapabilities{}, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux output platform is closed"))
	}
	return platform.root.native.destinationCapabilities()
}

func (platform *linuxPlatform) ProbeRecoverableFeatures() error {
	capabilities, err := platform.DestinationCapabilities()
	if err != nil {
		return err
	}
	if mode, modeErr := outputcap.SelectExecutionMode(capabilities); modeErr != nil ||
		mode != outputcap.ExecutionResumable {
		return errors.Join(outputcap.ErrRecoverableOutputUnsupported, modeErr)
	}
	return nil
}

func (*linuxPlatform) ValidateModifiedTime(modified catalog.ModifiedTime) error {
	return linuxError(linuxValidateModifiedTime(modified))
}

func (*linuxPlatform) CanonicalLocatorKey(path string) (string, error) {
	key, err := linuxOutputLocatorKey(path)
	return key, linuxError(err)
}

func (*linuxPlatform) CanonicalComponentKey(name string) (string, error) {
	if err := linuxValidateComponent("canonicalize output component", name); err != nil {
		return "", linuxError(err)
	}
	return name, nil
}

func (platform *linuxPlatform) Close() error {
	if platform == nil || platform.root == nil {
		return nil
	}
	err := platform.root.Close()
	platform.root = nil
	return linuxError(err)
}

func linuxError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errLinuxOutputUnsupported):
		return errors.Join(outputcap.ErrRecoverableOutputUnsupported, err)
	case errors.Is(err, errLinuxOutputUnsafe):
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	case errors.Is(err, errLinuxOutputCollision):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	case errors.Is(err, errLinuxOutputLockBusy):
		return errors.Join(outputcap.ErrNamespaceLockBusy, err)
	case errors.Is(err, fs.ErrExist):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	default:
		return err
	}
}

type linuxDestinationCapabilityReporter interface {
	DestinationCapabilities() (outputcap.DestinationCapabilities, error)
	LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile
}

type linuxSemanticPublisher interface {
	PublishFileNoReplace(outputcap.FileIdentity, string) (outputcap.PublishNoReplaceOutcome, error)
	ReservePublicDirectoryNoReplace(string) (
		outputcap.Directory,
		outputcap.PublishNoReplaceOutcome,
		error,
	)
}

type linuxLiveCleanupStageCreator interface {
	CreateLiveCleanupStage(outputcap.Directory, checkpointmodel.LiveCleanupTicket) error
}

type linuxLiveCleanupStageRemover interface {
	RemoveLiveCleanupStage(checkpointmodel.LiveCleanupTicket, outputcap.FileIdentity) error
}

var (
	_ outputcap.Platform                            = (*linuxPlatform)(nil)
	_ linuxDestinationCapabilityReporter            = (*linuxPlatform)(nil)
	_ linuxSemanticPublisher                        = (*linuxDirectory)(nil)
	_ linuxLiveCleanupStageCreator                  = (*linuxDirectory)(nil)
	_ linuxLiveCleanupStageRemover                  = (*linuxDirectory)(nil)
	_ outputcap.Directory                           = (*linuxDirectory)(nil)
	_ outputcap.PersistentDirectoryIdentity         = (*linuxDirectory)(nil)
	_ outputcap.PersistentDirectoryIdentityPreparer = (*linuxDirectory)(nil)
	_ outputcap.ObservedFile                        = (*linuxObservedFile)(nil)
	_ outputcap.RecoveryDurabilityFile              = (*linuxRecoveryDurabilityFile)(nil)
	_ outputcap.MutableFile                         = (*linuxMutableFile)(nil)
	_ outputcap.Lock                                = (*linuxLock)(nil)
)
