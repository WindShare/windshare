package outputnamespace

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// RootInspectionLimit detects one namespace entry beyond the supported root budget.
const RootInspectionLimit = 1024

// SessionIDSource is the only identity authority needed by namespace creation.
type SessionIDSource interface {
	NewOutputSessionID() (transfer.OutputSessionID, error)
}

// StateInstallEvent binds a low-level adopted cut to its resume namespace.
type StateInstallEvent struct {
	ResumeIntent transfer.ResumeIntent
	SessionID    transfer.OutputSessionID
	Cut          StateInstallCut
}

// Observer is the narrow observability port for durable namespace transitions.
type Observer interface {
	ObserveStateInstall(StateInstallEvent)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(StateInstallEvent)

func (observe ObserverFunc) ObserveStateInstall(event StateInstallEvent) {
	if observe != nil {
		observe(event)
	}
}

// ControllerConfig supplies policy values without granting live filesystem authority.
type ControllerConfig struct {
	Backend    transfer.OutputBackendID
	Random     io.Reader
	SessionIDs SessionIDSource
	Observer   Observer
}

// Controller coordinates control and session namespace state machines.
type Controller struct {
	backend    transfer.OutputBackendID
	random     io.Reader
	sessionIDs SessionIDSource
	observer   Observer
}

// NewController constructs a controller without retaining live filesystem authority.
func NewController(config ControllerConfig) Controller {
	return Controller{
		backend: config.Backend, random: config.Random,
		sessionIDs: config.SessionIDs, observer: config.Observer,
	}
}

// Store returns a record store whose observations retain their namespace identity.
func (controller Controller) Store(intent transfer.ResumeIntent, sessionID transfer.OutputSessionID) Store {
	var observer StateInstallObserver
	if controller.observer != nil {
		observer = StateInstallObserverFunc(func(cut StateInstallCut) {
			controller.observer.ObserveStateInstall(StateInstallEvent{
				ResumeIntent: intent, SessionID: sessionID, Cut: cut,
			})
		})
	}
	return NewStore(StoreConfig{Random: controller.random, Observer: observer})
}

func (controller Controller) newHeader(
	control resumestate.Control,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
	sessionID transfer.OutputSessionID,
) (resumestate.Header, error) {
	return resumestate.NewHeader(resumestate.HeaderSpec{
		Backend: controller.backend, SessionID: sessionID, Selection: selection,
		OutputRoot: control.OutputRoot(), OutputAncestry: ancestry,
	})
}

type ControlNamespace struct {
	directory outputcap.Directory
	sessions  outputcap.Directory
	control   resumestate.Control
}

func (namespace *ControlNamespace) Directory() outputcap.Directory { return namespace.directory }
func (namespace *ControlNamespace) Sessions() outputcap.Directory  { return namespace.sessions }
func (namespace *ControlNamespace) Control() resumestate.Control   { return namespace.control }

// ControlDisposition distinguishes an existing root from an installation completed by this open.
type ControlDisposition uint8

const (
	ControlExisting ControlDisposition = iota + 1
	ControlInstalled
)

// ControlOpenResult carries both live namespace authority and its durable opening cut.
type ControlOpenResult struct {
	Namespace   *ControlNamespace
	Disposition ControlDisposition
}

func (namespace *ControlNamespace) Close() error {
	if namespace == nil {
		return nil
	}
	return errors.Join(namespace.sessions.Close(), namespace.directory.Close())
}

func (controller Controller) OpenOrBootstrapControl(
	platform outputcap.Platform,
) (ControlOpenResult, error) {
	root := platform.Root()
	inspection, err := inspectControlRoot(root)
	if err != nil {
		return ControlOpenResult{}, err
	}
	if inspection.controlPresent {
		return controller.openExistingControl(root, platform, inspection.candidates)
	}
	recovery, err := controller.recoverBootstrapCandidates(root, platform, inspection.candidates)
	if err != nil {
		return ControlOpenResult{}, err
	}
	if recovery.recovered {
		return recovery.open, nil
	}
	return controller.bootstrapControl(root, platform)
}

type controlRootInspection struct {
	controlPresent bool
	candidates     []string
}

func inspectControlRoot(root outputcap.Directory) (controlRootInspection, error) {
	controlKind, err := ObserveExactEntry(root, resumestate.ControlDirectoryName)
	if err != nil {
		return controlRootInspection{}, RootFault("inspect output root", err)
	}
	candidates, err := root.NamesWithPrefix(resumestate.BootstrapCandidatePrefix, RootInspectionLimit)
	if err != nil {
		return controlRootInspection{}, RootFault("inspect bootstrap candidates", err)
	}
	for _, name := range candidates {
		if _, err := resumestate.ParseBootstrapCandidateName(name); err != nil {
			return controlRootInspection{}, RootFault("classify bootstrap candidate", err)
		}
	}
	return controlRootInspection{
		controlPresent: controlKind != outputcap.EntryAbsent,
		candidates:     candidates,
	}, nil
}

func (controller Controller) openExistingControl(
	root outputcap.Directory,
	platform outputcap.Platform,
	candidates []string,
) (ControlOpenResult, error) {
	namespace, err := controller.OpenInstalledControl(root, platform)
	if err != nil {
		return ControlOpenResult{}, err
	}
	for _, name := range candidates {
		if err := removeRecoverableBootstrapCandidate(controller, root, name, platform, &namespace.control); err != nil {
			_ = namespace.Close()
			return ControlOpenResult{}, RootFault("remove superseded bootstrap candidate", err)
		}
	}
	return ControlOpenResult{Namespace: namespace, Disposition: ControlExisting}, nil
}

type bootstrapRecoveryResult struct {
	open      ControlOpenResult
	recovered bool
}

type bootstrapCandidateRecovery uint8

const (
	bootstrapCandidateRemoved bootstrapCandidateRecovery = iota + 1
	bootstrapCandidateCompleted
)

func (controller Controller) recoverBootstrapCandidates(
	root outputcap.Directory,
	platform outputcap.Platform,
	candidates []string,
) (bootstrapRecoveryResult, error) {
	complete := make([]string, 0, len(candidates))
	for _, name := range candidates {
		recovery, err := controller.recoverBootstrapCandidate(root, platform, name)
		if err != nil {
			return bootstrapRecoveryResult{}, err
		}
		if recovery == bootstrapCandidateCompleted {
			complete = append(complete, name)
		}
	}
	if len(complete) == 0 {
		return bootstrapRecoveryResult{}, nil
	}
	open, err := controller.installRecoveredBootstrap(root, platform, complete)
	return bootstrapRecoveryResult{open: open, recovered: err == nil}, err
}

func (controller Controller) recoverBootstrapCandidate(
	root outputcap.Directory,
	platform outputcap.Platform,
	name string,
) (bootstrapCandidateRecovery, error) {
	candidate, control, state, err := inspectBootstrapCandidate(controller, root, name, platform)
	if err != nil {
		return 0, RootFault("inspect bootstrap candidate", err)
	}
	switch state {
	case resumestate.BootstrapCandidateEmpty:
		if err := removeBootstrapCandidate(root, name, candidate); err != nil {
			return 0, RootFault("remove empty bootstrap candidate", err)
		}
		return bootstrapCandidateRemoved, nil
	case resumestate.BootstrapCandidateValidPartial:
		if err := completeBootstrapCandidate(controller, candidate, control); err != nil {
			_ = candidate.Close()
			return 0, RootFault("complete bootstrap candidate", err)
		}
		_ = candidate.Close()
		return bootstrapCandidateCompleted, nil
	case resumestate.BootstrapCandidateComplete:
		_ = candidate.Close()
		return bootstrapCandidateCompleted, nil
	default:
		_ = candidate.Close()
		return 0, RootFault("classify bootstrap candidate", outputfault.ErrRootUnsafe)
	}
}

func (controller Controller) installRecoveredBootstrap(
	root outputcap.Directory,
	platform outputcap.Platform,
	complete []string,
) (ControlOpenResult, error) {
	slices.Sort(complete)
	installErr := installBootstrapCandidate(root, complete[0])
	collision := errors.Is(installErr, outputcap.ErrNamespaceCollision)
	if installErr != nil && !collision {
		return ControlOpenResult{}, RootFault("install recovered bootstrap candidate", installErr)
	}
	namespace, err := controller.OpenInstalledControl(root, platform)
	if err != nil {
		return ControlOpenResult{}, err
	}
	losers := complete[1:]
	if collision {
		losers = complete
	}
	if err := controller.removeLosingBootstrapCandidates(root, platform, namespace, losers); err != nil {
		_ = namespace.Close()
		return ControlOpenResult{}, err
	}
	return ControlOpenResult{Namespace: namespace, Disposition: ControlInstalled}, nil
}

func (controller Controller) removeLosingBootstrapCandidates(
	root outputcap.Directory,
	platform outputcap.Platform,
	namespace *ControlNamespace,
	losers []string,
) error {
	for _, name := range losers {
		if err := removeRecoverableBootstrapCandidate(controller, root, name, platform, &namespace.control); err != nil {
			return RootFault("remove losing bootstrap candidate", err)
		}
	}
	return nil
}

func (controller Controller) bootstrapControl(
	root outputcap.Directory,
	platform outputcap.Platform,
) (ControlOpenResult, error) {

	control, err := controller.newControl(platform)
	if err != nil {
		return ControlOpenResult{}, err
	}
	nonce, err := resumestate.GenerateBootstrapNonce(controller.random)
	if err != nil {
		return ControlOpenResult{}, RootFault("allocate bootstrap nonce", err)
	}
	name := resumestate.BootstrapCandidateName(nonce)
	candidate, err := root.CreateDirectory(name, true)
	if err != nil {
		return ControlOpenResult{}, RootFault("create bootstrap candidate", err)
	}
	if err := completeBootstrapCandidate(controller, candidate, control); err != nil {
		_ = candidate.Close()
		return ControlOpenResult{}, RootFault("build bootstrap candidate", err)
	}
	installErr := installBootstrapCandidate(root, name)
	collision := errors.Is(installErr, outputcap.ErrNamespaceCollision)
	if installErr != nil {
		if !collision {
			_ = candidate.Close()
			return ControlOpenResult{}, RootFault("install control namespace", installErr)
		}
	}
	_ = candidate.Close()
	namespace, err := controller.OpenInstalledControl(root, platform)
	if err != nil {
		return ControlOpenResult{}, err
	}
	if namespace.control != control {
		_ = namespace.Close()
		return ControlOpenResult{}, RootFault("verify installed control binding", outputfault.ErrRootUnsafe)
	}
	if collision {
		if err := removeRecoverableBootstrapCandidate(controller, root, name, platform, &namespace.control); err != nil {
			_ = namespace.Close()
			return ControlOpenResult{}, RootFault("remove colliding bootstrap candidate", err)
		}
	}
	return ControlOpenResult{Namespace: namespace, Disposition: ControlInstalled}, nil
}

func (controller Controller) newControl(platform outputcap.Platform) (resumestate.Control, error) {
	root, err := platform.RootBinding()
	if err != nil {
		return resumestate.Control{}, RootFault("bind output root identity", err)
	}
	control, err := resumestate.NewControl(resumestate.ControlSpec{
		Backend: controller.backend, OutputRoot: root, Certification: platform.Certification(),
		Durability: platform.Durability(), Generation: 1,
	})
	if err != nil {
		return resumestate.Control{}, RootFault("construct control state", err)
	}
	return control, nil
}

// OpenInstalledControl validates and opens the fixed control namespace without bootstrapping it.
func (controller Controller) OpenInstalledControl(
	root outputcap.Directory,
	platform outputcap.Platform,
) (*ControlNamespace, error) {
	directory, err := root.OpenDirectory(resumestate.ControlDirectoryName, true)
	if err != nil {
		return nil, RootFault("open control namespace", err)
	}
	control, err := controller.readAndValidateControl(directory, platform)
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	sessions, err := validateControlChildren(directory)
	if err != nil {
		_ = directory.Close()
		return nil, RootFault("validate control namespace", err)
	}
	return &ControlNamespace{directory: directory, sessions: sessions, control: control}, nil
}

func (controller Controller) readAndValidateControl(
	directory outputcap.Directory,
	platform outputcap.Platform,
) (resumestate.Control, error) {
	encoded, err := ReadRecord(directory, resumestate.ControlRecordName, resumestate.MaxControlStateBytes)
	if err != nil {
		return resumestate.Control{}, RootFault("read global control state", err)
	}
	control, err := resumestate.DecodeControl(encoded)
	rootBinding, bindingErr := platform.RootBinding()
	if err != nil || control.Backend() != controller.backend || control.Certification() != platform.Certification() ||
		control.Durability() != platform.Durability() || bindingErr != nil || control.OutputRoot() != rootBinding {
		return resumestate.Control{}, RootFault(
			"validate global control state", errors.Join(err, bindingErr, outputfault.ErrRootUnsafe),
		)
	}
	return control, nil
}

func validateControlChildren(directory outputcap.Directory) (outputcap.Directory, error) {
	names, err := directory.Names(4)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	expected := []string{resumestate.ControlRecordName, resumestate.CoordinatorLockName, resumestate.SessionsDirectoryName}
	slices.Sort(expected)
	if !slices.Equal(names, expected) {
		return nil, outputfault.ErrRootUnsafe
	}
	lock, err := directory.OpenFile(resumestate.CoordinatorLockName, true, false)
	if err != nil {
		return nil, err
	}
	size, sizeErr := lock.Size()
	closeErr := lock.Close()
	if sizeErr != nil || closeErr != nil || size != 0 {
		return nil, errors.Join(outputfault.ErrRootUnsafe, sizeErr, closeErr)
	}
	return directory.OpenDirectory(resumestate.SessionsDirectoryName, true)
}

func removeRecoverableBootstrapCandidate(
	controller Controller,
	root outputcap.Directory,
	name string,
	platform outputcap.Platform,
	installed *resumestate.Control,
) error {
	candidate, control, observation, err := inspectBootstrapCandidate(controller, root, name, platform)
	if err != nil {
		return err
	}
	if observation == resumestate.BootstrapCandidateUnsafe ||
		observation != resumestate.BootstrapCandidateEmpty && installed != nil && control != *installed {
		_ = candidate.Close()
		return outputfault.ErrRootUnsafe
	}
	return removeBootstrapCandidate(root, name, candidate)
}

func removeBootstrapCandidate(root outputcap.Directory, name string, candidate outputcap.Directory) error {
	if candidate == nil {
		return outputfault.ErrRootUnsafe
	}
	names, err := candidate.Names(4)
	if err != nil {
		return errors.Join(err, candidate.Close())
	}
	for _, child := range names {
		if child != resumestate.ControlRecordName && child != resumestate.CoordinatorLockName &&
			child != resumestate.SessionsDirectoryName {
			return errors.Join(outputfault.ErrRootUnsafe, candidate.Close())
		}
	}

	// control.state is the candidate's envelope. Removing it first would turn a
	// crash during cleanup into an unclassifiable non-empty directory. Retiring
	// data before the stable lock and the envelope last ensures every persisted
	// cut is either a recognized partial candidate or an empty removable shell.
	for _, child := range []string{
		resumestate.SessionsDirectoryName,
		resumestate.CoordinatorLockName,
		resumestate.ControlRecordName,
	} {
		if !slices.Contains(names, child) {
			continue
		}
		if err := VerifyPinnedDirectoryEntry(root, name, candidate); err != nil {
			return errors.Join(err, candidate.Close())
		}
		if err := removeBootstrapChild(candidate, child); err != nil {
			return errors.Join(err, candidate.Close())
		}
		if err := candidate.Sync(); err != nil {
			return errors.Join(err, candidate.Close())
		}
	}

	if err := VerifyPinnedDirectoryEntry(root, name, candidate); err != nil {
		return errors.Join(err, candidate.Close())
	}
	remaining, err := candidate.Names(1)
	if err != nil || len(remaining) != 0 {
		return errors.Join(outputfault.ErrRootUnsafe, err, candidate.Close())
	}
	if err := root.RemoveDirectory(name, candidate); err != nil {
		return errors.Join(err, candidate.Close())
	}
	return errors.Join(root.Sync(), candidate.Close())
}

func removeBootstrapChild(candidate outputcap.Directory, name string) error {
	switch name {
	case resumestate.SessionsDirectoryName:
		directory, err := candidate.OpenDirectory(name, true)
		if err != nil {
			return err
		}
		children, listErr := directory.Names(1)
		if listErr != nil || len(children) != 0 {
			return errors.Join(outputfault.ErrRootUnsafe, listErr, directory.Close())
		}
		return errors.Join(candidate.RemoveDirectory(name, directory), directory.Close())
	case resumestate.CoordinatorLockName, resumestate.ControlRecordName:
		file, err := candidate.OpenFile(name, true, false)
		if err != nil {
			return err
		}
		return errors.Join(candidate.RemoveFile(name, file), file.Close())
	default:
		return outputfault.ErrRootUnsafe
	}
}

func VerifyPinnedDirectoryEntry(parent outputcap.Directory, name string, expected outputcap.Directory) error {
	current, err := parent.OpenDirectory(name, true)
	if err != nil {
		return err
	}
	same, compareErr := current.SameDirectory(expected)
	closeErr := current.Close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(outputfault.ErrRootUnsafe, compareErr, closeErr)
	}
	return nil
}

func RootFault(operation string, cause error) error {
	if errors.Is(cause, outputcap.ErrRecoverableOutputUnsupported) {
		return outputfault.New(transfer.OutputFaultRoot, transfer.OutputFaultUnsupportedFilesystem,
			errors.Join(outputfault.ErrUnsupportedVolume, fmt.Errorf("%s: %w", operation, cause)))
	}
	return outputfault.New(transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
		errors.Join(outputfault.ErrRootUnsafe, fmt.Errorf("%s: %w", operation, cause)))
}
