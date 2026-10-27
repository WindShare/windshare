//go:build windows

package outputwindows

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func TestWindowsOperationalErrorContextPreservesRawTaxonomy(t *testing.T) {
	rawCauses := []error{
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_SHARING_VIOLATION,
		windows.ERROR_FILE_NOT_FOUND,
		errors.New("generic operational failure"),
	}
	for _, cause := range rawCauses {
		t.Run(cause.Error(), func(t *testing.T) {
			native := windowsNativeOperationFailure("exercise native operation", "entry", cause)
			requireWindowsRawOperationalError(t, native, cause)
			if message := native.Error(); !strings.Contains(message, "exercise native operation") ||
				!strings.Contains(message, "entry") {
				t.Fatalf("operational context = %q", message)
			}
			requireWindowsRawOperationalError(t, windowsOutputError(native), cause)
		})
	}

	for _, cause := range []error{
		windows.ERROR_INVALID_PARAMETER,
		windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_CALL_NOT_IMPLEMENTED,
	} {
		native := windowsNativeOperationFailure("exercise unsupported operation", "entry", cause)
		if !errors.Is(native, errWindowsOutputUnsupported) ||
			!errors.Is(windowsOutputError(native), outputcap.ErrRecoverableOutputUnsupported) {
			t.Fatalf("unsupported cause %v = %v", cause, native)
		}
	}

	for _, cause := range []error{windows.ERROR_FILE_EXISTS, windows.ERROR_ALREADY_EXISTS} {
		native := windowsNativeNoReplaceFailure("exercise no-replace operation", "entry", cause)
		if !errors.Is(native, errWindowsOutputCollision) ||
			!errors.Is(windowsOutputError(native), outputcap.ErrNamespaceCollision) {
			t.Fatalf("collision cause %v = %v", cause, native)
		}
	}

	observed := windowsFailure(
		"verify positively observed entry", "entry", errWindowsOutputUnsafe, windows.ERROR_ACCESS_DENIED,
	)
	if !errors.Is(windowsOutputError(observed), outputcap.ErrUnsafeNamespace) {
		t.Fatalf("positive-entry denial lost unsafe evidence: %v", observed)
	}
}

func TestWindowsDirectoryInstallDeniedClassificationRequiresSettledObservation(t *testing.T) {
	closeFailure := errors.New("close collision observation")
	tests := []struct {
		name            string
		observationErr  error
		closeErr        error
		nativeCategory  error
		wrapperCategory error
		rawCause        error
	}{
		{
			name:            "settled positive observation is collision",
			nativeCategory:  errWindowsOutputCollision,
			wrapperCategory: outputcap.ErrNamespaceCollision,
		},
		{
			name:     "close failure prevents collision settlement",
			closeErr: closeFailure,
			rawCause: closeFailure,
		},
		{
			name:            "unsafe positive observation remains unsafe",
			observationErr:  windowsFailure("observe existing directory", "target", errWindowsOutputUnsafe, windows.ERROR_ACCESS_DENIED),
			nativeCategory:  errWindowsOutputUnsafe,
			wrapperCategory: outputcap.ErrUnsafeNamespace,
		},
		{
			name:           "missing observation remains operational",
			observationErr: windows.ERROR_FILE_NOT_FOUND,
			rawCause:       windows.ERROR_FILE_NOT_FOUND,
		},
		{
			name:            "unsupported observation remains unsupported",
			observationErr:  windows.ERROR_NOT_SUPPORTED,
			nativeCategory:  errWindowsOutputUnsupported,
			wrapperCategory: outputcap.ErrRecoverableOutputUnsupported,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			native := windowsDirectoryInstallDeniedFailure(
				"target", windows.ERROR_ACCESS_DENIED, test.observationErr, test.closeErr,
			)
			wrapped := windowsOutputError(native)
			if test.rawCause != nil {
				requireWindowsRawOperationalError(t, native, test.rawCause)
				requireWindowsRawOperationalError(t, wrapped, test.rawCause)
			} else if !errors.Is(native, test.nativeCategory) || !errors.Is(wrapped, test.wrapperCategory) {
				t.Fatalf("classification = native %v wrapped %v", native, wrapped)
			}
			if !errors.Is(native, windows.ERROR_ACCESS_DENIED) {
				t.Fatalf("classification lost native install denial: %v", native)
			}
		})
	}
}

func TestWindowsRealNativeOpenFailuresRemainRawThroughWrapper(t *testing.T) {
	platform := openWindowsTestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	wrapper := &windowsOutputDirectory{native: root}

	for _, open := range []struct {
		name string
		run  func() error
	}{
		{name: "native", run: func() error {
			_, err := root.OpenRegularFile("missing.bin")
			return err
		}},
		{name: "wrapper", run: func() error {
			_, err := wrapper.OpenObservedFile("missing.bin", false)
			return err
		}},
		{name: "native pinned entry", run: func() error {
			_, err := root.openPinnedEntry("missing.bin")
			return err
		}},
		{name: "wrapper pinned entry", run: func() error {
			_, err := wrapper.OpenEntry("missing.bin")
			return err
		}},
	} {
		t.Run("missing "+open.name, func(t *testing.T) {
			err := open.run()
			requireWindowsRawOperationalError(t, err, fs.ErrNotExist)
		})
	}

	const blockedName = "share-blocked.bin"
	blocked, err := root.CreatePrivateFile(blockedName)
	if err != nil {
		t.Fatal(err)
	}
	if err := blocked.Close(); err != nil {
		t.Fatal(err)
	}
	blocker, _, err := windowsOpenNativeWithOptions(
		root.handle(), blockedName, windowsReadFileAccess(), windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE, windows.FILE_ATTRIBUTE_NORMAL, nil, 0,
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(blocker)

	for _, open := range []struct {
		name string
		run  func() error
	}{
		{name: "native", run: func() error {
			_, err := root.OpenRegularFile(blockedName)
			return err
		}},
		{name: "wrapper", run: func() error {
			_, err := wrapper.OpenObservedFile(blockedName, false)
			return err
		}},
	} {
		t.Run("sharing "+open.name, func(t *testing.T) {
			err := open.run()
			requireWindowsRawOperationalError(t, err, windows.ERROR_SHARING_VIOLATION)
		})
	}

	const collisionName = "collision.bin"
	existing, err := root.CreatePrivateFile(collisionName)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	if duplicate, err := root.CreatePrivateFile(collisionName); duplicate != nil ||
		!errors.Is(err, errWindowsOutputCollision) {
		t.Fatalf("native collision = file %v error %v", duplicate, err)
	}
	if duplicate, err := wrapper.CreateFile(collisionName, true, 0); duplicate != nil ||
		!errors.Is(err, outputcap.ErrNamespaceCollision) {
		t.Fatalf("wrapper collision = file %v error %v", duplicate, err)
	}

	inaccessibleRoot := *root
	inaccessibleRoot.inspector = windowsObjectInspectorFunc(func(windows.Handle) (windowsObjectFacts, error) {
		return windowsObjectFacts{}, windows.ERROR_ACCESS_DENIED
	})
	if opened, err := inaccessibleRoot.OpenRegularFile(collisionName); opened != nil {
		_ = opened.Close()
		t.Fatal("native post-open denial unexpectedly returned an authority")
	} else if !errors.Is(err, errWindowsOutputUnsafe) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("native post-open denial = %v", err)
	}
	inaccessibleWrapper := &windowsOutputDirectory{native: &inaccessibleRoot}
	if opened, err := inaccessibleWrapper.OpenObservedFile(collisionName, false); opened != nil {
		_ = opened.Close()
		t.Fatal("wrapper post-open denial unexpectedly returned an authority")
	} else if !errors.Is(err, outputcap.ErrUnsafeNamespace) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("wrapper post-open denial = %v", err)
	}
	if _, err := inaccessibleWrapper.ObserveEntry(collisionName); !errors.Is(err, outputcap.ErrUnsafeNamespace) ||
		!errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("wrapper positive observation denial = %v", err)
	}
}

func TestWindowsRealNativeMutationFailuresRemainOperational(t *testing.T) {
	platform := openWindowsTestPlatform(t)
	defer platform.Close()
	root := platform.Root()
	wrapper := &windowsOutputDirectory{native: root}

	const sourceName = "read-only-source.bin"
	created, err := root.CreatePrivateFile(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := root.OpenRegularFile(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	wrappedReadOnly := &windowsOutputObservedFile{State: &windowsOutputFileState{
		native: readOnly, private: true, borrowed: true,
	}}

	staleDirectory, err := root.Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(staleDirectory.handle()); err != nil {
		t.Fatal(err)
	}
	staleWrapper := &windowsOutputDirectory{native: staleDirectory}
	if duplicate, err := staleDirectory.Duplicate(); duplicate != nil {
		_ = duplicate.Close()
		t.Fatal("stale native authority unexpectedly duplicated")
	} else {
		requireWindowsRawOperationalError(t, err, windows.ERROR_INVALID_HANDLE)
	}
	if duplicate, err := staleWrapper.Duplicate(); duplicate != nil {
		_ = duplicate.Close()
		t.Fatal("stale wrapper authority unexpectedly duplicated")
	} else {
		requireWindowsRawOperationalError(t, err, windows.ERROR_INVALID_HANDLE)
	}
	_, err = staleDirectory.observeEntry("denied-observation.bin")
	requireWindowsRawOperationalError(t, err, windows.ERROR_INVALID_HANDLE)
	_, err = staleWrapper.ObserveEntry("denied-wrapper-observation.bin")
	requireWindowsRawOperationalError(t, err, windows.ERROR_INVALID_HANDLE)
	if linked, err := staleDirectory.LinkRegularFileNoReplace(readOnly, "denied-link.bin"); linked != nil {
		_ = linked.Close()
		t.Fatal("stale destination authority unexpectedly authorized a native hard link")
	} else {
		requireWindowsRawOperationalError(t, err, windows.ERROR_INVALID_HANDLE)
	}
	if linked, err := staleWrapper.LinkFileNoReplace(wrappedReadOnly, "denied-wrapper-link.bin"); linked != nil {
		_ = linked.Close()
		t.Fatal("stale destination authority unexpectedly authorized a wrapper hard link")
	} else {
		requireWindowsRawOperationalError(t, err, windows.ERROR_INVALID_HANDLE)
	}
	_ = staleDirectory.file.Close()
	staleDirectory.file = nil
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	requireWindowsRawOperationalError(t,
		windowsSetHandleModifiedTime(readOnly.handle(), readOnly.path, modified),
		windows.ERROR_ACCESS_DENIED,
	)
	requireWindowsRawOperationalError(t, windowsRemoveHandle(readOnly.handle()), windows.ERROR_ACCESS_DENIED)
	requireWindowsRawOperationalError(t,
		root.AtomicReplacePrivateFile(readOnly, "denied-replacement.State"),
		windows.ERROR_ACCESS_DENIED,
	)
	requireWindowsRawOperationalError(t,
		wrapper.ReplacePrivateFile(wrappedReadOnly, "denied-wrapper-replacement.State"),
		windows.ERROR_ACCESS_DENIED,
	)

	publicSource, err := root.openDirectory("read-only-directory", false, windows.FILE_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	defer publicSource.Close()
	if installed, err := root.InstallPrivateDirectoryNoReplace(publicSource, "denied-directory-install"); installed != nil {
		_ = installed.Close()
		t.Fatal("public placement handle unexpectedly authorized a native rename")
	} else {
		requireWindowsRawOperationalError(t, err, windows.ERROR_ACCESS_DENIED)
	}
	if installed, err := wrapper.InstallDirectoryNoReplace(
		&windowsOutputDirectory{native: publicSource}, "denied-wrapper-directory-install",
	); installed != nil {
		_ = installed.Close()
		t.Fatal("public placement handle unexpectedly authorized a wrapper rename")
	} else {
		requireWindowsRawOperationalError(t, err, windows.ERROR_ACCESS_DENIED)
	}

	requireWindowsRawOperationalError(t,
		windowsRemoveHandle(windows.InvalidHandle), windows.ERROR_INVALID_HANDLE,
	)
}

func TestWindowsRealNativeLockTaxonomy(t *testing.T) {
	platform := openWindowsTestPlatform(t)
	defer platform.Close()
	root := platform.Root()

	const name = "lock-taxonomy.bin"
	created, err := root.CreatePrivateFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	first, err := root.AcquireExistingStableLock(name)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := root.AcquireExistingStableLock(name); second != nil ||
		!errors.Is(err, errWindowsOutputLockBusy) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("contended native lock = lock %v error %v", second, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	handle, _, err := windowsOpenNative(
		root.handle(), name, windows.SYNCHRONIZE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE, windows.FILE_ATTRIBUTE_NORMAL, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	minimal := &windowsFile{
		file: os.NewFile(uintptr(handle), name), path: name, volume: root.volume,
		inspector: root.inspector, policy: root.policy,
	}
	if minimal.file == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("minimal lock handle could not be wrapped")
	}
	if lock, err := windowsLockStableFile(minimal, name); lock != nil {
		_ = lock.Close()
		t.Fatal("metadata-only handle unexpectedly authorized a byte-range lock")
	} else {
		requireWindowsRawOperationalError(t, err, windows.ERROR_ACCESS_DENIED)
	}
}

func requireWindowsRawOperationalError(t *testing.T, err error, cause error) {
	t.Helper()
	if err == nil || !errors.Is(err, cause) {
		t.Fatalf("operational error = %v, want raw cause %v", err, cause)
	}
	for _, category := range []error{
		errWindowsOutputUnsupported,
		errWindowsOutputUnsafe,
		errWindowsOutputCollision,
		errWindowsOutputLockBusy,
		outputcap.ErrRecoverableOutputUnsupported,
		outputcap.ErrUnsafeNamespace,
		outputcap.ErrNamespaceCollision,
		outputcap.ErrNamespaceLockBusy,
	} {
		if errors.Is(err, category) {
			t.Fatalf("raw operational error %v acquired semantic category %v", err, category)
		}
	}
}
