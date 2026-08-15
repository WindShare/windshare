//go:build linux

package outputlinux

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

type linuxV3Platform struct {
	rootOpenDisposition outputcap.RootOpenDisposition
	root                *linuxV3Directory
}

type linuxV3DirectoryOrigin struct {
	parent *linuxOutputDirectory
	name   string
}

type linuxV3Directory struct {
	native *linuxOutputDirectory
	origin *linuxV3DirectoryOrigin
}

type linuxV3FileOrigin struct {
	parent *linuxOutputDirectory
	name   string
}

type linuxV3File struct {
	native   *linuxOutputRegularFile
	origin   *linuxV3FileOrigin
	private  bool
	borrowed bool
}

type linuxV3Lock struct {
	native *linuxOutputStableLock
	file   *linuxV3File
}

type linuxV3EntryRef struct {
	native *linuxOutputPinnedEntry
}

func Open(path string, create bool) (outputcap.Platform, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace,
			linuxUnsafe("open output root", "output root must be absolute", nil))
	}
	clean := filepath.Clean(path)
	rootOpenDisposition := outputcap.CallerProvidedContainer
	root, err := linuxOpenExt4OutputRoot(clean, &linuxHostOutputSystem)
	if create && errors.Is(err, fs.ErrNotExist) {
		root, err = linuxCreateCertifiedOutputRoot(clean)
		if err == nil {
			rootOpenDisposition = outputcap.AuthorityCreatedRoot
		}
	}
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return &linuxV3Platform{
		rootOpenDisposition: rootOpenDisposition,
		root:                &linuxV3Directory{native: root},
	}, nil
}

func linuxCreateCertifiedOutputRoot(path string) (_ *linuxOutputDirectory, resultErr error) {
	const operation = "create certified output root"
	candidate := path
	missing := make([]string, 0, 4)
	var current *linuxOutputDirectory
	for {
		opened, err := linuxOpenExt4OutputRoot(candidate, &linuxHostOutputSystem)
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
				"no existing certified ancestor contains the requested root", nil), err)
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
	reopened, err := linuxOpenExt4OutputRoot(path, &linuxHostOutputSystem)
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

func (platform *linuxV3Platform) Root() outputcap.Directory {
	if platform == nil {
		return nil
	}
	return platform.root
}

func (platform *linuxV3Platform) RootOpenDisposition() outputcap.RootOpenDisposition {
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

func (platform *linuxV3Platform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux output platform is closed"))
	}
	if err := platform.root.native.validatePublicCreateAuthority(); err != nil {
		return nil, linuxV3Error(err)
	}
	root, err := platform.root.Duplicate()
	if err != nil {
		return nil, err
	}
	return &linuxOutputPublicOperationGuard{root: root}, nil
}

func (*linuxV3Platform) Certification() outputcap.CertificationID {
	return outputcap.CertificationLinuxExt4ProcessRestart
}

func (platform *linuxV3Platform) RootBinding() (outputcap.OutputRootBinding, error) {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux output platform is closed"),
		)
	}
	root := platform.root.native
	if err := root.verifyHandle(); err != nil {
		return outputcap.OutputRootBinding{}, linuxV3Error(err)
	}
	certificate := root.certificate
	volume, err := linuxEncodeMountIdentity(certificate.mount)
	if err != nil {
		return outputcap.OutputRootBinding{}, linuxV3Error(err)
	}
	if root.system.restartIdentity == nil {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrRecoverableOutputUnsupported,
			errors.New("osfs: Linux directory restart-identity provider is unavailable"),
		)
	}
	restartIdentity, err := root.system.restartIdentity.Read(root.system, root.fd, certificate.mount)
	if err != nil {
		return outputcap.OutputRootBinding{}, linuxV3Error(err)
	}
	if !restartIdentity.matchesHandle(root.object) ||
		!restartIdentity.sameDirectory(certificate.rootRestartIdentity) {
		return outputcap.OutputRootBinding{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux output-root restart identity changed"),
		)
	}
	object, err := linuxEncodeDirectoryRestartIdentity(restartIdentity)
	if err != nil {
		return outputcap.OutputRootBinding{}, linuxV3Error(err)
	}
	binding, err := outputcap.NewOutputRootBinding(platform.Certification(), volume, object)
	return binding, linuxV3Error(err)
}

func (*linuxV3Platform) Durability() transfer.DurabilityLevel {
	return transfer.DurabilityProcessRestart
}

func (*linuxV3Platform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	return checkpointmodel.LiveCleanupLinuxExt4V1
}

func (platform *linuxV3Platform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return outputcap.DestinationCapabilities{}, errors.Join(
			outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux output platform is closed"))
	}
	return platform.root.native.destinationCapabilities()
}

func (platform *linuxV3Platform) ProbeRecoverableFeatures() error {
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

func (*linuxV3Platform) ValidateModifiedTime(modified catalog.ModifiedTime) error {
	return linuxV3Error(linuxValidateModifiedTime(modified))
}

func (*linuxV3Platform) CanonicalLocatorKey(path string) (string, error) {
	key, err := linuxOutputLocatorKey(path)
	return key, linuxV3Error(err)
}

func (*linuxV3Platform) CanonicalComponentKey(name string) (string, error) {
	if err := linuxValidateComponent("canonicalize output component", name); err != nil {
		return "", linuxV3Error(err)
	}
	return name, nil
}

func (platform *linuxV3Platform) Close() error {
	if platform == nil || platform.root == nil {
		return nil
	}
	err := platform.root.Close()
	platform.root = nil
	return linuxV3Error(err)
}

func linuxV3Error(err error) error {
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
	PublishFileNoReplace(outputcap.File, string) (outputcap.PublishNoReplaceOutcome, error)
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
	RemoveLiveCleanupStage(checkpointmodel.LiveCleanupTicket, outputcap.File) error
}

var (
	_ outputcap.Platform                            = (*linuxV3Platform)(nil)
	_ linuxDestinationCapabilityReporter            = (*linuxV3Platform)(nil)
	_ linuxSemanticPublisher                        = (*linuxV3Directory)(nil)
	_ linuxLiveCleanupStageCreator                  = (*linuxV3Directory)(nil)
	_ linuxLiveCleanupStageRemover                  = (*linuxV3Directory)(nil)
	_ outputcap.Directory                           = (*linuxV3Directory)(nil)
	_ outputcap.PersistentDirectoryIdentity         = (*linuxV3Directory)(nil)
	_ outputcap.PersistentDirectoryIdentityPreparer = (*linuxV3Directory)(nil)
	_ outputcap.File                                = (*linuxV3File)(nil)
	_ outputcap.Lock                                = (*linuxV3Lock)(nil)
)
