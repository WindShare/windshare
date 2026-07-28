//go:build windows

package outputwindows

import (
	"errors"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsV3PersistentObjectID [16]byte

func (identity windowsV3PersistentObjectID) valid() bool {
	return identity != windowsV3PersistentObjectID{}
}

type windowsV3FileObjectIDBuffer struct {
	ObjectID      windowsV3PersistentObjectID
	BirthVolumeID [16]byte
	BirthObjectID [16]byte
	DomainID      [16]byte
}

// The provider deliberately exposes no read, set, or delete operation. NTFS's
// idempotent CreateOrGet transition is the only supported way to install or
// recover a durable directory identity.
type windowsV3PersistentObjectIDProvider interface {
	CreateOrGet(windows.Handle) (windowsV3PersistentObjectID, error)
}

type nativeWindowsV3PersistentObjectIDProvider struct{}

func (nativeWindowsV3PersistentObjectIDProvider) CreateOrGet(
	handle windows.Handle,
) (windowsV3PersistentObjectID, error) {
	return windowsV3CreateOrGetPersistentObjectID(handle)
}

func windowsV3CreateOrGetPersistentObjectID(handle windows.Handle) (windowsV3PersistentObjectID, error) {
	var buffer windowsV3FileObjectIDBuffer
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
		return windowsV3PersistentObjectID{}, err
	}
	if returned != uint32(unsafe.Sizeof(buffer)) || !buffer.ObjectID.valid() {
		return windowsV3PersistentObjectID{}, errors.New("NTFS returned an invalid persistent object-ID envelope")
	}
	return buffer.ObjectID, nil
}

// Reopened authorities have independent state, while true duplicated handles
// share it. File IDs are deliberately not cache keys: NTFS may reuse one after
// deletion, so every fresh handle must execute CreateOrGet before it can claim
// persistent identity.
type windowsV3PersistentObjectIDState struct {
	mu       sync.RWMutex
	identity windowsV3PersistentObjectID
}

func newWindowsV3PersistentObjectIDState() *windowsV3PersistentObjectIDState {
	return &windowsV3PersistentObjectIDState{}
}

func (state *windowsV3PersistentObjectIDState) current() (windowsV3PersistentObjectID, bool) {
	if state == nil {
		return windowsV3PersistentObjectID{}, false
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.identity, state.identity.valid()
}

func (directory *windowsV3Directory) cachedPersistentObjectID() (
	windowsV3PersistentObjectID,
	bool,
	error,
) {
	if err := directory.usable(); err != nil {
		return windowsV3PersistentObjectID{}, false, err
	}
	if directory.objectIDState == nil {
		return windowsV3PersistentObjectID{}, false, errors.New("persistent NTFS object identity cache is absent")
	}
	facts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return windowsV3PersistentObjectID{}, false, err
	}
	if err := windowsV3ValidateOpenedObject(facts, directory.volume, true); err != nil {
		return windowsV3PersistentObjectID{}, false, err
	}
	identity, found := directory.objectIDState.current()
	return identity, found, nil
}

func (directory *windowsV3Directory) createOrGetPersistentObjectID(
	operation string,
	authorize func() error,
) (windowsV3PersistentObjectID, error) {
	if err := directory.usable(); err != nil {
		return windowsV3PersistentObjectID{}, err
	}
	if directory.objectIDs == nil || directory.objectIDState == nil {
		return windowsV3PersistentObjectID{}, windowsV3Failure(
			operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("persistent NTFS object-ID authority is absent"),
		)
	}
	if authorize == nil {
		return windowsV3PersistentObjectID{}, windowsV3Failure(
			operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("persistent NTFS object-ID operation has no authority policy"),
		)
	}

	state := directory.objectIDState
	state.mu.Lock()
	defer state.mu.Unlock()
	before, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return windowsV3PersistentObjectID{}, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe, err)
	}
	if err := windowsV3ValidateOpenedObject(before, directory.volume, true); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe, err)
	}
	if err := authorize(); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3Failure(
			operation, directory.path, windowsV3AuthorityFailureClass(err), err,
		)
	}

	identity, err := directory.objectIDs.CreateOrGet(directory.handle())
	if err != nil {
		state.identity = windowsV3PersistentObjectID{}
		return windowsV3PersistentObjectID{}, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsupported, err)
	}
	if !identity.valid() {
		state.identity = windowsV3PersistentObjectID{}
		return windowsV3PersistentObjectID{}, windowsV3Failure(
			operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("persistent NTFS object-ID provider returned a zero identity"),
		)
	}
	if state.identity.valid() && state.identity != identity {
		state.identity = windowsV3PersistentObjectID{}
		return windowsV3PersistentObjectID{}, windowsV3Failure(
			operation, directory.path, errWindowsV3OutputUnsafe,
			errors.New("persistent NTFS object ID changed after authority was fixed"),
		)
	}

	after, err := directory.inspector.Inspect(directory.handle())
	if err != nil || !before.object.same(after.object) {
		state.identity = windowsV3PersistentObjectID{}
		return windowsV3PersistentObjectID{}, windowsV3Failure(
			operation, directory.path, errWindowsV3OutputUnsafe,
			errors.Join(errors.New("directory incarnation changed while preparing persistent identity"), err),
		)
	}
	if err := windowsV3ValidateOpenedObject(after, directory.volume, true); err != nil {
		state.identity = windowsV3PersistentObjectID{}
		return windowsV3PersistentObjectID{}, windowsV3Failure(operation, directory.path, errWindowsV3OutputUnsafe, err)
	}
	if err := authorize(); err != nil {
		state.identity = windowsV3PersistentObjectID{}
		return windowsV3PersistentObjectID{}, windowsV3Failure(
			operation, directory.path, windowsV3AuthorityFailureClass(err), err,
		)
	}

	state.identity = identity
	return identity, nil
}

func (directory *windowsV3Directory) preparePrivatePersistentObjectID() (windowsV3PersistentObjectID, error) {
	return directory.createOrGetPersistentObjectID(
		"prepare private persistent output object identity",
		func() error { return directory.verify(true) },
	)
}

func (root *windowsV3Directory) preparePersistentRootIdentity() (resultErr error) {
	const operation = "prepare persistent output-root identity"
	identity, err := root.prepareIdentityClaim()
	if err != nil {
		return err
	}
	if err := root.Sync(); err != nil {
		return err
	}
	reopened, err := openWindowsV3OutputPlatform(root.path)
	if err != nil {
		return windowsV3Failure(operation, root.path, errWindowsV3OutputUnsafe, err)
	}
	defer func() { resultErr = errors.Join(resultErr, reopened.Close()) }()
	reopenedGuard, err := reopened.acquirePublicOperationGuard()
	if err != nil {
		return windowsV3Failure(operation, root.path, errWindowsV3OutputUnsafe, err)
	}
	defer func() { resultErr = errors.Join(resultErr, reopenedGuard.Close()) }()
	reopenedRoot := reopenedGuard.Root()

	// The scoped caller already pins root placement. The absolute reopen proves
	// that restart lookup reaches the same current File ID before CreateOrGet is
	// allowed to recover the persistent ID on the independent authority.
	same, err := sameWindowsV3OpenedDirectory(root, reopenedRoot)
	if err != nil || !same {
		return windowsV3Failure(operation, root.path, errWindowsV3OutputUnsafe,
			errors.Join(errors.New("reopened output root has a different current incarnation"), err))
	}
	reopenedIdentity, err := reopenedRoot.prepareIdentityClaim()
	if err != nil {
		return err
	}
	if string(reopenedIdentity) != string(identity) {
		return windowsV3Failure(operation, root.path, errWindowsV3OutputUnsafe,
			errors.New("reopened output root has a different persistent incarnation"))
	}
	return nil
}
