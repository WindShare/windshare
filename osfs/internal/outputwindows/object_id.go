//go:build windows

package outputwindows

import (
	"errors"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsPersistentObjectID [16]byte

func (identity windowsPersistentObjectID) valid() bool {
	return identity != windowsPersistentObjectID{}
}

type windowsFileObjectIDBuffer struct {
	ObjectID      windowsPersistentObjectID
	BirthVolumeID [16]byte
	BirthObjectID [16]byte
	DomainID      [16]byte
}

// The provider deliberately exposes no read, set, or delete operation. NTFS's
// idempotent CreateOrGet transition is the only supported way to install or
// recover a durable directory identity.
type windowsPersistentObjectIDProvider interface {
	CreateOrGet(windows.Handle) (windowsPersistentObjectID, error)
}

type nativeWindowsPersistentObjectIDProvider struct{}

func (nativeWindowsPersistentObjectIDProvider) CreateOrGet(
	handle windows.Handle,
) (windowsPersistentObjectID, error) {
	return windowsCreateOrGetPersistentObjectID(handle)
}

func windowsCreateOrGetPersistentObjectID(handle windows.Handle) (windowsPersistentObjectID, error) {
	var buffer windowsFileObjectIDBuffer
	var returned uint32
	if err := windows.DeviceIoControl(
		handle,
		windows.FSCTL_CREATE_OR_GET_OBJECT_ID,
		nil,
		0,
		(*byte)(unsafe.Pointer(&buffer)),
		uint32(unsafe.Sizeof(buffer)),
		&returned,
		nil,
	); err != nil {
		return windowsPersistentObjectID{}, err
	}
	if returned != uint32(unsafe.Sizeof(buffer)) || !buffer.ObjectID.valid() {
		return windowsPersistentObjectID{}, errors.New("NTFS returned an invalid persistent object-ID envelope")
	}
	return buffer.ObjectID, nil
}

// Reopened authorities have independent state, while true duplicated handles
// share it. File IDs are deliberately not cache keys: NTFS may reuse one after
// deletion, so every fresh handle must execute CreateOrGet before it can claim
// persistent identity.
type windowsPersistentObjectIDState struct {
	mu       sync.RWMutex
	identity windowsPersistentObjectID
}

func newWindowsPersistentObjectIDState() *windowsPersistentObjectIDState {
	return &windowsPersistentObjectIDState{}
}

func (state *windowsPersistentObjectIDState) current() (windowsPersistentObjectID, bool) {
	if state == nil {
		return windowsPersistentObjectID{}, false
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.identity, state.identity.valid()
}

func (directory *windowsDirectory) cachedPersistentObjectID() (
	windowsPersistentObjectID,
	bool,
	error,
) {
	if err := directory.usable(); err != nil {
		return windowsPersistentObjectID{}, false, err
	}
	if directory.objectIDState == nil {
		return windowsPersistentObjectID{}, false, errors.New("persistent NTFS object identity cache is absent")
	}
	facts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return windowsPersistentObjectID{}, false, err
	}
	if err := windowsValidateOpenedObject(facts, directory.volume, true); err != nil {
		return windowsPersistentObjectID{}, false, err
	}
	identity, found := directory.objectIDState.current()
	return identity, found, nil
}

func (directory *windowsDirectory) createOrGetPersistentObjectID(
	operation string,
	authorize func() error,
) (windowsPersistentObjectID, error) {
	if err := directory.usable(); err != nil {
		return windowsPersistentObjectID{}, err
	}
	if directory.objectIDs == nil || directory.objectIDState == nil {
		return windowsPersistentObjectID{}, windowsFailure(
			operation, directory.path, errWindowsOutputUnsafe,
			errors.New("persistent NTFS object-ID authority is absent"),
		)
	}
	if authorize == nil {
		return windowsPersistentObjectID{}, windowsFailure(
			operation, directory.path, errWindowsOutputUnsafe,
			errors.New("persistent NTFS object-ID operation has no authority policy"),
		)
	}

	state := directory.objectIDState
	state.mu.Lock()
	defer state.mu.Unlock()
	before, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return windowsPersistentObjectID{}, windowsFailure(operation, directory.path, errWindowsOutputUnsafe, err)
	}
	if err := windowsValidateOpenedObject(before, directory.volume, true); err != nil {
		return windowsPersistentObjectID{}, windowsFailure(operation, directory.path, errWindowsOutputUnsafe, err)
	}
	if err := authorize(); err != nil {
		return windowsPersistentObjectID{}, windowsFailure(
			operation, directory.path, windowsAuthorityFailureClass(err), err,
		)
	}

	identity, err := directory.objectIDs.CreateOrGet(directory.handle())
	if err != nil {
		state.identity = windowsPersistentObjectID{}
		return windowsPersistentObjectID{}, windowsFailure(operation, directory.path, errWindowsOutputUnsupported, err)
	}
	if !identity.valid() {
		state.identity = windowsPersistentObjectID{}
		return windowsPersistentObjectID{}, windowsFailure(
			operation, directory.path, errWindowsOutputUnsafe,
			errors.New("persistent NTFS object-ID provider returned a zero identity"),
		)
	}
	if state.identity.valid() && state.identity != identity {
		state.identity = windowsPersistentObjectID{}
		return windowsPersistentObjectID{}, windowsFailure(
			operation, directory.path, errWindowsOutputUnsafe,
			errors.New("persistent NTFS object ID changed after authority was fixed"),
		)
	}

	after, err := directory.inspector.Inspect(directory.handle())
	if err != nil || !before.object.same(after.object) {
		state.identity = windowsPersistentObjectID{}
		return windowsPersistentObjectID{}, windowsFailure(
			operation, directory.path, errWindowsOutputUnsafe,
			errors.Join(errors.New("directory incarnation changed while preparing persistent identity"), err),
		)
	}
	if err := windowsValidateOpenedObject(after, directory.volume, true); err != nil {
		state.identity = windowsPersistentObjectID{}
		return windowsPersistentObjectID{}, windowsFailure(operation, directory.path, errWindowsOutputUnsafe, err)
	}
	if err := authorize(); err != nil {
		state.identity = windowsPersistentObjectID{}
		return windowsPersistentObjectID{}, windowsFailure(
			operation, directory.path, windowsAuthorityFailureClass(err), err,
		)
	}

	state.identity = identity
	return identity, nil
}

func (directory *windowsDirectory) preparePrivatePersistentObjectID() (windowsPersistentObjectID, error) {
	return directory.createOrGetPersistentObjectID(
		"prepare private persistent output object identity",
		func() error { return directory.verify(true) },
	)
}

func (root *windowsDirectory) preparePersistentRootIdentity() (resultErr error) {
	const operation = "prepare persistent output-root identity"
	identity, err := root.prepareIdentityClaim()
	if err != nil {
		return err
	}
	if err := root.Sync(); err != nil {
		return err
	}
	reopened, err := openWindowsOutputPlatform(root.path)
	if err != nil {
		return windowsFailure(operation, root.path, errWindowsOutputUnsafe, err)
	}
	defer func() { resultErr = errors.Join(resultErr, reopened.Close()) }()
	reopenedGuard, err := reopened.acquirePublicOperationGuard()
	if err != nil {
		return windowsFailure(operation, root.path, errWindowsOutputUnsafe, err)
	}
	defer func() { resultErr = errors.Join(resultErr, reopenedGuard.Close()) }()
	reopenedRoot := reopenedGuard.Root()

	// The scoped caller already pins root placement. The absolute reopen proves
	// that restart lookup reaches the same current File ID before CreateOrGet is
	// allowed to recover the persistent ID on the independent authority.
	same, err := sameWindowsOpenedDirectory(root, reopenedRoot)
	if err != nil || !same {
		return windowsFailure(operation, root.path, errWindowsOutputUnsafe,
			errors.Join(errors.New("reopened output root has a different current incarnation"), err))
	}
	reopenedIdentity, err := reopenedRoot.prepareIdentityClaim()
	if err != nil {
		return err
	}
	if string(reopenedIdentity) != string(identity) {
		return windowsFailure(operation, root.path, errWindowsOutputUnsafe,
			errors.New("reopened output root has a different persistent incarnation"))
	}
	return nil
}
