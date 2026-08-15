package destinationauthority

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"strings"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const (
	controlLifecycleNamespace      = "lifecycle-v1"
	controlParticipantsDirectory   = "participants"
	controlCoordinatorLock         = "coordinator.lock"
	controlParticipantLockSuffix   = ".lock"
	controlParticipantNonceBytes   = 16
	maximumControlParticipants     = 1024
	maximumControlBindAttempts     = 8
	maximumCoordinatorLockAttempts = 64
	coordinatorLockRetryInterval   = time.Millisecond
)

// PrivateStateRecycler removes only application-owned, terminal private state.
// Returning false preserves the control namespace because recovery or unknown
// state still exists; it is not an operational failure.
type PrivateStateRecycler func(outputcap.Directory) (bool, error)

// controlUseLease is a process-lifetime presence proof. The short coordinator
// lock serializes registration and final recycling without serializing downloads.
type controlUseLease struct {
	participantName string
	participant     outputcap.Lock
}

func bindControlUse(
	root outputcap.Directory,
	enabled bool,
	nonceSource io.Reader,
) (outputcap.Directory, bool, *controlUseLease, error) {
	if !enabled {
		control, created, err := openOrCreateExactPrivateDirectory(root, controlDirectoryName)
		return control, created, nil, err
	}
	if nonceSource == nil {
		nonceSource = rand.Reader
	}
	for range maximumControlBindAttempts {
		control, created, err := openOrCreateExactPrivateDirectory(root, controlDirectoryName)
		if err != nil {
			return nil, false, nil, err
		}
		lease, current, leaseErr := beginControlUse(root, control, nonceSource)
		if leaseErr != nil {
			return nil, false, nil, errors.Join(leaseErr, control.Close())
		}
		if current {
			return control, created, lease, nil
		}
		if err := errors.Join(abortControlUse(control, lease), control.Close()); err != nil {
			return nil, false, nil, err
		}
	}
	return nil, false, nil, ErrControlNamespaceChanged
}

func beginControlUse(
	root outputcap.Directory,
	control outputcap.Directory,
	nonceSource io.Reader,
) (_ *controlUseLease, current bool, resultErr error) {
	lifecycle, lifecycleCreated, err := openOrCreateExactPrivateDirectory(control, controlLifecycleNamespace)
	if err != nil {
		return nil, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, lifecycle.Close()) }()
	participants, participantsCreated, err := openOrCreateExactPrivateDirectory(
		lifecycle, controlParticipantsDirectory,
	)
	if err != nil {
		return nil, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, participants.Close()) }()
	if lifecycleCreated || participantsCreated {
		if err := errors.Join(participants.Sync(), lifecycle.Sync(), control.Sync()); err != nil {
			return nil, false, err
		}
	}
	coordinator, created, err := acquireControlCoordinator(lifecycle)
	if err != nil {
		return nil, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, coordinator.Close()) }()
	if created {
		if err := lifecycle.Sync(); err != nil {
			return nil, false, err
		}
	}
	current, err = controlMatchesRoot(root, control)
	if err != nil || !current {
		return nil, current, err
	}
	if err := authenticateControlLifecycle(lifecycle, participants); err != nil {
		return nil, false, err
	}
	lease, err := createControlParticipant(participants, nonceSource)
	if err != nil {
		return nil, false, err
	}
	return lease, true, nil
}

func acquireControlCoordinator(directory outputcap.Directory) (outputcap.Lock, bool, error) {
	for range maximumCoordinatorLockAttempts {
		lock, created, err := directory.AcquireLock(controlCoordinatorLock, false)
		if err == nil {
			if lock == nil {
				return nil, false, outputcap.ErrUnsafeNamespace
			}
			return lock, created, nil
		}
		if !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
			return nil, false, errors.Join(err, closeLock(lock))
		}
		runtime.Gosched()
		time.Sleep(coordinatorLockRetryInterval)
	}
	return nil, false, outputcap.ErrNamespaceLockBusy
}

func authenticateControlLifecycle(
	lifecycle outputcap.Directory,
	participants outputcap.Directory,
) error {
	names, err := lifecycle.Names(3)
	if err != nil || len(names) > 2 {
		return errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	for _, name := range names {
		var expected outputcap.EntryKind
		switch name {
		case controlCoordinatorLock:
			expected = outputcap.EntryRegularFile
		case controlParticipantsDirectory:
			expected = outputcap.EntryDirectory
		default:
			return outputcap.ErrUnsafeNamespace
		}
		kind, exact, classifyErr := lifecycle.ClassifyExactEntry(name)
		if classifyErr != nil || !exact || kind != expected {
			return errors.Join(classifyErr, outputcap.ErrUnsafeNamespace)
		}
	}
	participantNames, err := participants.Names(maximumControlParticipants + 1)
	if err != nil || len(participantNames) > maximumControlParticipants {
		return errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	for _, name := range participantNames {
		kind, exact, classifyErr := participants.ClassifyExactEntry(name)
		if classifyErr != nil || !exact || kind != outputcap.EntryRegularFile || !validControlParticipantName(name) {
			return errors.Join(classifyErr, outputcap.ErrUnsafeNamespace)
		}
	}
	return nil
}

func createControlParticipant(
	participants outputcap.Directory,
	nonceSource io.Reader,
) (*controlUseLease, error) {
	for range maximumControlBindAttempts {
		nonce := make([]byte, controlParticipantNonceBytes)
		if _, err := io.ReadFull(nonceSource, nonce); err != nil {
			return nil, err
		}
		name := hex.EncodeToString(nonce) + controlParticipantLockSuffix
		lock, created, err := participants.AcquireLock(name, false)
		if err != nil {
			return nil, errors.Join(err, closeLock(lock))
		}
		if !created {
			if err := closeLock(lock); err != nil {
				return nil, err
			}
			continue
		}
		if err := participants.Sync(); err != nil {
			return nil, errors.Join(err, lock.Close())
		}
		return &controlUseLease{participantName: name, participant: lock}, nil
	}
	return nil, outputcap.ErrNamespaceCollision
}

func validControlParticipantName(name string) bool {
	hexName, found := strings.CutSuffix(name, controlParticipantLockSuffix)
	if !found || len(hexName) != controlParticipantNonceBytes*2 || hexName != strings.ToLower(hexName) {
		return false
	}
	_, err := hex.DecodeString(hexName)
	return err == nil
}

func controlMatchesRoot(root outputcap.Directory, expected outputcap.Directory) (bool, error) {
	kind, exact, err := root.ClassifyExactEntry(controlDirectoryName)
	if err != nil || !exact || kind == outputcap.EntryAbsent {
		return false, err
	}
	if kind != outputcap.EntryDirectory {
		return false, nil
	}
	current, err := openExactPrivateDirectory(root, controlDirectoryName)
	if err != nil {
		return false, err
	}
	same, sameErr := current.SameDirectory(expected)
	return same, errors.Join(sameErr, current.Close())
}

func abortControlUse(control outputcap.Directory, lease *controlUseLease) (resultErr error) {
	if lease == nil || lease.participant == nil {
		return nil
	}
	// A failed bind must stop carrying liveness immediately. If coordinated
	// removal is unavailable, the unlocked marker is deliberately recoverable by
	// the next successful binder.
	defer func() { lease.participant = nil }()
	lifecycle, err := openExactPrivateDirectory(control, controlLifecycleNamespace)
	if err != nil {
		return errors.Join(lease.participant.Close(), err)
	}
	defer func() { resultErr = errors.Join(resultErr, lifecycle.Close()) }()
	participants, err := openExactPrivateDirectory(lifecycle, controlParticipantsDirectory)
	if err != nil {
		return errors.Join(lease.participant.Close(), err)
	}
	defer func() { resultErr = errors.Join(resultErr, participants.Close()) }()
	coordinator, _, err := acquireControlCoordinator(lifecycle)
	if err != nil {
		return lease.participant.Close()
	}
	defer func() { resultErr = errors.Join(resultErr, coordinator.Close()) }()
	removeErr := participants.RemoveFile(lease.participantName, lease.participant.File())
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	if removeErr == nil {
		removeErr = participants.Sync()
	}
	return errors.Join(removeErr, lease.participant.Close())
}

func (authority *BoundDestination) recycleControlState() (resultErr error) {
	if authority.controlUse == nil {
		return nil
	}
	if authority.platform == nil || authority.rootWitness == nil || authority.control == nil || authority.recycler == nil {
		return abortControlUse(authority.control, authority.controlUse)
	}
	guard, err := authority.platform.AcquirePublicOperationGuard()
	if err != nil || guard == nil || guard.Root() == nil {
		return errors.Join(err, abortControlUse(authority.control, authority.controlUse), closeGuard(guard))
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	root := guard.Root()
	same, sameErr := root.SameDirectory(authority.rootWitness)
	if sameErr != nil || !same {
		return errors.Join(ErrRetainedRootChanged, sameErr, abortControlUse(authority.control, authority.controlUse))
	}
	return authority.controlUse.releaseAndRecycle(root, authority.control, authority.recycler)
}

func (lease *controlUseLease) releaseAndRecycle(
	root outputcap.Directory,
	control outputcap.Directory,
	recycler PrivateStateRecycler,
) (resultErr error) {
	if lease == nil || lease.participant == nil || recycler == nil {
		return nil
	}
	lifecycle, err := openExactPrivateDirectory(control, controlLifecycleNamespace)
	if err != nil {
		return errors.Join(err, lease.participant.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, closeDirectory(lifecycle)) }()
	participants, err := openExactPrivateDirectory(lifecycle, controlParticipantsDirectory)
	if err != nil {
		return errors.Join(err, lease.participant.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, closeDirectory(participants)) }()
	coordinator, _, err := acquireControlCoordinator(lifecycle)
	if err != nil {
		// Another current binder will eventually become responsible for recycling.
		return lease.participantClose()
	}
	defer func() { resultErr = errors.Join(resultErr, closeLock(coordinator)) }()
	current, err := controlMatchesRoot(root, control)
	if err != nil || !current {
		return errors.Join(err, lease.removeOwnParticipant(participants))
	}
	if err := authenticateControlLifecycle(lifecycle, participants); err != nil {
		return errors.Join(err, lease.removeOwnParticipant(participants))
	}
	if err := lease.removeOwnParticipant(participants); err != nil {
		return err
	}
	last, err := removeStaleControlParticipants(participants)
	if err != nil || !last {
		return err
	}
	eligible, err := recycler(control)
	if err != nil || !eligible {
		return err
	}
	if clean, err := controlContainsOnlyLifecycle(control); err != nil || !clean {
		return err
	}
	if err := removeExactEmptyDirectory(lifecycle, controlParticipantsDirectory, participants); err != nil {
		return err
	}
	participants = nil
	removeCoordinatorErr := lifecycle.RemoveFile(controlCoordinatorLock, coordinator.File())
	if removeCoordinatorErr == nil {
		removeCoordinatorErr = lifecycle.Sync()
	}
	if removeCoordinatorErr != nil {
		return removeCoordinatorErr
	}
	if err := coordinator.Close(); err != nil {
		return err
	}
	coordinator = nil
	if err := removeExactEmptyDirectory(control, controlLifecycleNamespace, lifecycle); err != nil {
		return err
	}
	lifecycle = nil
	if err := root.RemoveDirectory(controlDirectoryName, control); err != nil {
		return err
	}
	return root.Sync()
}

func (lease *controlUseLease) participantClose() error {
	if lease == nil || lease.participant == nil {
		return nil
	}
	err := lease.participant.Close()
	lease.participant = nil
	return err
}

func (lease *controlUseLease) removeOwnParticipant(participants outputcap.Directory) error {
	if lease == nil || lease.participant == nil {
		return nil
	}
	removeErr := participants.RemoveFile(lease.participantName, lease.participant.File())
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	if removeErr == nil {
		removeErr = participants.Sync()
	}
	return errors.Join(removeErr, lease.participantClose())
}

func removeStaleControlParticipants(participants outputcap.Directory) (bool, error) {
	names, err := participants.Names(maximumControlParticipants + 1)
	if err != nil || len(names) > maximumControlParticipants {
		return false, err
	}
	for _, name := range names {
		if !validControlParticipantName(name) {
			return false, nil
		}
		lock, _, lockErr := participants.AcquireLock(name, true)
		if errors.Is(lockErr, outputcap.ErrNamespaceLockBusy) {
			return false, nil
		}
		if lockErr != nil || lock == nil {
			return false, errors.Join(lockErr, closeLock(lock))
		}
		removeErr := participants.RemoveFile(name, lock.File())
		closeErr := lock.Close()
		if removeErr != nil || closeErr != nil {
			return false, errors.Join(removeErr, closeErr)
		}
	}
	if len(names) != 0 {
		if err := participants.Sync(); err != nil {
			return false, err
		}
	}
	return true, nil
}

func controlContainsOnlyLifecycle(control outputcap.Directory) (bool, error) {
	names, err := control.Names(2)
	if errors.Is(err, outputcap.ErrUnsafeNamespace) {
		// The bounded overflow itself proves that application or unknown state
		// remains; close is not an authorization to classify it as a failure.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect recyclable control namespace: %w", err)
	}
	if len(names) != 1 || names[0] != controlLifecycleNamespace {
		return false, nil
	}
	kind, exact, err := control.ClassifyExactEntry(controlLifecycleNamespace)
	return err == nil && exact && kind == outputcap.EntryDirectory, err
}

func removeExactEmptyDirectory(
	parent outputcap.Directory,
	name string,
	directory outputcap.Directory,
) error {
	if parent == nil || directory == nil {
		return outputcap.ErrUnsafeNamespace
	}
	names, err := directory.Names(1)
	if err != nil || len(names) != 0 {
		return errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	removeErr := parent.RemoveDirectory(name, directory)
	if removeErr == nil {
		removeErr = parent.Sync()
	}
	return errors.Join(removeErr, directory.Close())
}

func closeGuard(guard outputcap.PublicOperationGuard) error {
	if guard == nil {
		return nil
	}
	return guard.Close()
}

func closeLock(lock outputcap.Lock) error {
	if lock == nil {
		return nil
	}
	return lock.Close()
}
