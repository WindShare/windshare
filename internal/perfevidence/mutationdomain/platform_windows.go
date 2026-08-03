//go:build windows

package mutationdomain

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/windshare/windshare/internal/perfevidence/mutationdomain/windowsbroker"
	"golang.org/x/sys/windows"
)

const (
	brokerArgument          = "--perfevidence-mutation-broker"
	brokerImageHandlePrefix = "--perfevidence-broker-image-handle="
)

var (
	ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
	isProcessInJob  = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	windowsTarget   struct {
		sync.Mutex
		active *windowsTargetLifecycle
	}
)

type windowsTargetLifecycle struct {
	job      windows.Handle
	identity appContainerIdentity
}

type platformOutputAuthority struct {
	handle    windows.Handle
	identity  windows.ByHandleFileInformation
	promotion *windowsPromotionAuthority
	closed    bool
}

type windowsOutputRevision struct {
	attributes    uint32
	creation      windows.Filetime
	lastWrite     windows.Filetime
	volume        uint32
	sizeHigh      uint32
	sizeLow       uint32
	links         uint32
	fileIndexHigh uint32
	fileIndexLow  uint32
}

func maybeRunPlatformBroker(arguments []string, stdin io.Reader, stdout, stderr io.Writer) (bool, int) {
	if len(arguments) != 2 || arguments[0] != brokerArgument ||
		!strings.HasPrefix(arguments[1], brokerImageHandlePrefix) {
		return false, 0
	}
	value := strings.TrimPrefix(arguments[1], brokerImageHandlePrefix)
	handleValue, err := strconv.ParseUint(value, 10, 64)
	if err == nil && handleValue == 0 {
		err = errors.New("retained broker image handle is invalid")
	}
	if err == nil {
		image := os.NewFile(uintptr(handleValue), "retained-broker-image")
		if image == nil {
			err = errors.New("retained broker image handle is unavailable")
		} else {
			err = windowsbroker.Run(stdin, stdout, image, stageSealedInputs)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "private mutation broker failed: %v\n", err)
		return true, 1
	}
	return true, 0
}

func maybeRunPlatformTarget([]string, io.Writer) (bool, int) { return false, 0 }

func platformTargetInvocation(executable string, arguments []string) (string, []string) {
	return executable, arguments
}

func preparePlatformTarget(process *exec.Cmd) (func() error, func() error, func() error, error) {
	if process == nil {
		return nil, nil, nil, errors.New("Windows target command is unavailable")
	}
	identity, err := currentAppContainerIdentity()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("retain helper AppContainer identity: %w", err)
	}
	if process.SysProcAttr == nil {
		process.SysProcAttr = helperTargetProcessAttributes()
	}
	job, err := windowsbroker.NewKillOnCloseJob()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create per-command Windows job: %w", err)
	}
	lifecycle := &windowsTargetLifecycle{job: job, identity: identity}
	windowsTarget.Lock()
	if windowsTarget.active != nil {
		windowsTarget.Unlock()
		return nil, nil, nil, errors.Join(
			errors.New("previous Windows target job has not settled"),
			windows.CloseHandle(job),
		)
	}
	windowsTarget.active = lifecycle
	windowsTarget.Unlock()
	afterStart := func() error {
		if process.Process == nil {
			return errors.New("Windows target process did not start")
		}
		var operationErr error
		handleErr := process.Process.WithHandle(func(raw uintptr) {
			handle := windows.Handle(raw)
			if err := windowsbroker.SealKernelHandleDACL(handle, windowsbroker.AppContainerProcessDescriptor(
				lifecycle.identity.traditionalUserSID,
				lifecycle.identity.isolationCapabilitySID,
			)); err != nil {
				operationErr = fmt.Errorf("seal suspended Windows target process DACL: %w", err)
				return
			}
			if err := windowsbroker.VerifyPrivateProcess(handle, windowsBrokerIdentity(lifecycle.identity)); err != nil {
				operationErr = fmt.Errorf("attest suspended Windows target token: %w", err)
				return
			}
			if err := windows.AssignProcessToJobObject(lifecycle.job, handle); err != nil {
				operationErr = fmt.Errorf("assign suspended Windows target to per-command job: %w", err)
				return
			}
			if err := verifyWindowsProcessInJob(handle, lifecycle.job); err != nil {
				operationErr = err
				return
			}
			if err := windowsbroker.VerifyLauncherReopenDenied(uint32(process.Process.Pid)); err != nil {
				operationErr = err
				return
			}
		})
		return errors.Join(operationErr, handleErr)
	}
	releaseTarget := func() error {
		if process.Process == nil {
			return errors.New("Windows target process did not start")
		}
		var resumeErr error
		handleErr := process.Process.WithHandle(func(raw uintptr) {
			status, _, _ := ntResumeProcess.Call(raw)
			if int32(status) < 0 {
				resumeErr = fmt.Errorf("resume contained Windows target: NTSTATUS 0x%08x", uint32(status))
			}
		})
		return errors.Join(resumeErr, handleErr)
	}
	closeTargetGate := func() error {
		return settleRegisteredWindowsTarget(lifecycle)
	}
	return afterStart, releaseTarget, closeTargetGate, nil
}

func verifyWindowsProcessInJob(process, job windows.Handle) error {
	var contained int32
	result, _, callErr := isProcessInJob.Call(
		uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&contained)),
	)
	if result == 0 {
		return fmt.Errorf("query suspended Windows target job membership: %w", callErr)
	}
	if contained == 0 {
		return errors.New("suspended Windows target is not in its per-command job")
	}
	return nil
}

func settleRegisteredWindowsTarget(expected *windowsTargetLifecycle) error {
	windowsTarget.Lock()
	if windowsTarget.active != expected {
		windowsTarget.Unlock()
		return errors.New("Windows target job ownership changed before settlement")
	}
	windowsTarget.active = nil
	windowsTarget.Unlock()
	jobErr := windowsbroker.SettleJob(expected.job)
	expected.job = 0
	expected.identity = appContainerIdentity{}
	return jobErr
}

func openPlatformSession(ctx context.Context, configuration initialization) (*session, error) {
	profileLedger, err := openWindowsProfileLedger(configuration.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	if err := profileLedger.recover(); err != nil {
		return nil, errors.Join(err, profileLedger.close())
	}
	brokerImage, err := windowsbroker.CreateRetainedImage(configuration.RuntimeRoot)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create retained private mutation broker: %w", err), profileLedger.close())
	}
	var closePlatformOnce sync.Once
	var closePlatformErr error
	closeImage := func() error {
		closePlatformOnce.Do(func() {
			closePlatformErr = errors.Join(brokerImage.Close(), profileLedger.recover(), profileLedger.close())
		})
		return closePlatformErr
	}
	inherited, err := windowsbroker.DuplicateInheritableHandle(windows.Handle(brokerImage.File().Fd()))
	if err != nil {
		return nil, errors.Join(err, closeImage())
	}
	arguments := []string{
		brokerArgument,
		brokerImageHandlePrefix + strconv.FormatUint(uint64(inherited), 10),
	}
	environment := make([]string, 0, 5)
	for _, name := range []string{"SystemRoot", "WINDIR", "USERPROFILE", "LOCALAPPDATA", "APPDATA"} {
		environment = append(environment, name+"="+os.Getenv(name))
	}
	started, err := windowsbroker.StartProcess(windowsbroker.ProcessSpec{
		Executable:        brokerImage.Path(),
		Arguments:         arguments,
		Environment:       environment,
		Directory:         configuration.RuntimeRoot,
		ProcessDescriptor: windowsbroker.BrokerProcessDescriptor,
		ThreadDescriptor:  windowsbroker.BrokerThreadDescriptor,
		Inherited:         []windows.Handle{inherited},
		OwnJob:            true,
		Suspended:         true,
	})
	inheritedCloseErr := windows.CloseHandle(inherited)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("start sealed private mutation broker: %w", err), inheritedCloseErr, closeImage())
	}
	fail := func(operationErr error) (*session, error) {
		cleanupErr := errors.Join(started.Kill(), started.ClosePipes(), started.Wait(), closeImage())
		stderrText := started.Stderr().Snapshot()
		var stderrErr error
		if len(stderrText) > 0 {
			stderrErr = errors.New(string(stderrText))
		}
		return nil, errors.Join(operationErr, stderrErr, cleanupErr)
	}
	if inheritedCloseErr != nil {
		return fail(inheritedCloseErr)
	}
	if err := windowsbroker.VerifyProcessImage(started.Handle(), brokerImage.File(), brokerImage.Path(), true); err != nil {
		return fail(fmt.Errorf("verify retained private mutation broker image: %w", err))
	}
	if err := started.Resume(); err != nil {
		return fail(fmt.Errorf("resume sealed private mutation broker: %w", err))
	}
	initializationDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = started.Kill()
		case <-initializationDone:
		}
	}()
	defer close(initializationDone)
	if err := writeJSONLine(started.Stdin(), configuration); err != nil {
		return fail(err)
	}
	reader := bufio.NewReaderSize(started.Stdout(), maximumProtocolLine)
	var ready response
	if err := readJSONLine(reader, &ready); err != nil {
		return fail(errors.Join(err, ctx.Err()))
	}
	if ready.Error != "" {
		return fail(errors.New(ready.Error))
	}
	stderrCapture := &limitedBuffer{limit: maximumCapturedBytes, capture: started.Stderr()}
	return &session{
		stdin: started.Stdin(), stdout: reader, stdoutPipe: started.Stdout(), stderr: stderrCapture,
		kill: started.Kill, wait: started.Wait, closePlatform: closeImage,
		resolveProcessID: func(processID int) (int, error) {
			return resolvePlatformProcessID(int(started.ProcessID()), processID)
		},
	}, nil
}

func preparePlatformHelper(configuration initialization) (
	string,
	map[string]string,
	func() error,
	error,
) {
	if configuration.PrivateRoot == "" {
		return "", nil, nil, errors.New("AppContainer private root is missing")
	}
	if err := materializeWindowsBootstrap(configuration); err != nil {
		return "", nil, nil, err
	}
	sources := make(map[string]string, len(configuration.Roots))
	for _, root := range configuration.Roots {
		sources[root.Name] = filepath.Join(configuration.PrivateRoot, "bootstrap", root.Name)
	}
	cleanup := func() error {
		return cleanupWindowsPrivateChildren(configuration.PrivateRoot, []string{
			privateInputDirectory, privateOutputDirectory, privateCacheDirectory,
			privateTemporaryDirectory, privatePromotedDirectory, "bootstrap",
		})
	}
	return configuration.PrivateRoot, sources, cleanup, nil
}

func helperTargetProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_SUSPENDED}
}

func resolvePlatformProcessID(_ int, processID int) (int, error) {
	if processID <= 0 {
		return 0, errors.New("Windows target process identity is invalid")
	}
	return processID, nil
}

func settlePlatformTarget() error {
	windowsTarget.Lock()
	active := windowsTarget.active
	windowsTarget.Unlock()
	if active == nil {
		return errors.New("Windows target job was not retained through command settlement")
	}
	return settleRegisteredWindowsTarget(active)
}

func openPlatformOutputAuthority(path string) (*platformOutputAuthority, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, errors.Join(errors.New("isolated output authority is not a no-follow directory"), err, windows.CloseHandle(handle))
	}
	authority := &platformOutputAuthority{handle: handle, identity: information}
	if filepath.Base(path) == privateOutputDirectory {
		authority.promotion, err = openWindowsPromotionAuthority(
			filepath.Join(filepath.Dir(path), privatePromotedDirectory),
		)
		if err != nil {
			return nil, errors.Join(err, windows.CloseHandle(handle))
		}
	}
	return authority, nil
}

func (authority *platformOutputAuthority) close() error {
	if authority == nil || authority.closed {
		return nil
	}
	authority.closed = true
	return errors.Join(authority.promotion.close(), windows.CloseHandle(authority.handle))
}

func (authority *platformOutputAuthority) verify() error {
	if authority == nil || authority.closed {
		return errors.New("isolated output directory authority is closed")
	}
	var observed windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(authority.handle, &observed); err != nil {
		return err
	}
	if !sameWindowsObject(authority.identity, observed) || observed.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		observed.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("isolated output directory authority changed")
	}
	return nil
}

func platformOpenProtectedOutput(
	authority *platformOutputAuthority,
	leaf string,
) (*os.File, func() error, error) {
	if filepath.Base(leaf) != leaf || leaf == "." || leaf == ".." {
		return nil, nil, fmt.Errorf("isolated output leaf %q is invalid", leaf)
	}
	if err := authority.verify(); err != nil {
		return nil, nil, err
	}
	handle, information, err := openRelativeWindowsOutput(authority.handle, leaf)
	if err != nil {
		return nil, nil, err
	}
	revision := windowsOutputRevisionOf(information)
	verify := func() error {
		if err := authority.verify(); err != nil {
			return err
		}
		var handleInformation windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &handleInformation); err != nil {
			return err
		}
		if windowsOutputRevisionOf(handleInformation) != revision {
			return errors.New("isolated output revision changed while framing")
		}
		named, namedInformation, err := openRelativeWindowsOutput(authority.handle, leaf)
		if err != nil {
			return err
		}
		closeErr := windows.CloseHandle(named)
		if !sameWindowsObject(information, namedInformation) {
			return errors.Join(errors.New("isolated output name no longer identifies the framed file"), closeErr)
		}
		return closeErr
	}
	return os.NewFile(uintptr(handle), leaf), verify, nil
}

func openRelativeWindowsOutput(
	root windows.Handle,
	leaf string,
) (windows.Handle, windows.ByHandleFileInformation, error) {
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil ||
		information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		information.NumberOfLinks != 1 {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, errors.Join(
			errors.New("isolated output is not a single-link no-follow regular file"), err, windows.CloseHandle(handle),
		)
	}
	return handle, information, nil
}

func sameWindowsObject(left, right windows.ByHandleFileInformation) bool {
	return left.VolumeSerialNumber == right.VolumeSerialNumber &&
		left.FileIndexHigh == right.FileIndexHigh && left.FileIndexLow == right.FileIndexLow
}

func windowsOutputRevisionOf(information windows.ByHandleFileInformation) windowsOutputRevision {
	return windowsOutputRevision{
		attributes: information.FileAttributes, creation: information.CreationTime,
		lastWrite: information.LastWriteTime, volume: information.VolumeSerialNumber,
		sizeHigh: information.FileSizeHigh, sizeLow: information.FileSizeLow,
		links: information.NumberOfLinks, fileIndexHigh: information.FileIndexHigh,
		fileIndexLow: information.FileIndexLow,
	}
}

const maximumWindowsBootstrapManifestBytes = 256 << 20

func materializeWindowsBootstrap(configuration initialization) (resultErr error) {
	if configuration.BootstrapManifest == "" {
		return errors.New("sealed AppContainer bootstrap manifest is missing")
	}
	manifestLeaf, err := filepath.Rel(configuration.PrivateRoot, configuration.BootstrapManifest)
	if err != nil || filepath.Base(manifestLeaf) != manifestLeaf {
		return errors.Join(errors.New("sealed AppContainer bootstrap manifest is outside private authority"), err)
	}
	rootAuthority, err := openPlatformOutputAuthority(configuration.PrivateRoot)
	if err != nil {
		return fmt.Errorf("retain sealed AppContainer bootstrap root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, rootAuthority.close()) }()
	manifestFile, verifyManifest, err := platformOpenProtectedOutput(rootAuthority, manifestLeaf)
	if err != nil {
		return err
	}
	info, err := manifestFile.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximumWindowsBootstrapManifestBytes {
		return errors.Join(errors.New("sealed AppContainer bootstrap manifest exceeded its bound"), err, manifestFile.Close())
	}
	var manifest windowsBootstrapManifest
	decodeErr := json.NewDecoder(io.LimitReader(manifestFile, info.Size()+1)).Decode(&manifest)
	closeErr := errors.Join(verifyManifest(), manifestFile.Close())
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return err
	}
	expectedRoots := make(map[string]rootSpec, len(configuration.Roots))
	for _, root := range configuration.Roots {
		expectedRoots[root.Name] = root
	}
	if len(manifest.Roots) != len(expectedRoots) {
		return errors.New("sealed AppContainer bootstrap root count is invalid")
	}
	bootstrapRoot := filepath.Join(configuration.PrivateRoot, "bootstrap")
	if err := os.Mkdir(bootstrapRoot, 0o700); err != nil {
		return err
	}
	seenRoots := make(map[string]bool, len(manifest.Roots))
	for _, name := range manifest.Roots {
		if _, expected := expectedRoots[name]; !expected || seenRoots[name] || filepath.Base(name) != name {
			return fmt.Errorf("sealed AppContainer bootstrap root %q is invalid", name)
		}
		seenRoots[name] = true
		if err := os.Mkdir(filepath.Join(bootstrapRoot, name), 0o700); err != nil {
			return err
		}
	}
	seenEntries := make(map[string]bool, len(manifest.Directories)+len(manifest.Files))
	sort.Slice(manifest.Directories, func(left, right int) bool {
		leftDepth := strings.Count(filepath.Clean(manifest.Directories[left].Relative), string(filepath.Separator))
		rightDepth := strings.Count(filepath.Clean(manifest.Directories[right].Relative), string(filepath.Separator))
		if leftDepth == rightDepth {
			return manifest.Directories[left].Relative < manifest.Directories[right].Relative
		}
		return leftDepth < rightDepth
	})
	for _, entry := range manifest.Directories {
		if !seenRoots[entry.Root] || !filepath.IsLocal(entry.Relative) || filepath.IsAbs(entry.Relative) ||
			entry.Relative == "." {
			return fmt.Errorf("sealed AppContainer bootstrap directory %q is invalid", entry.Relative)
		}
		destination := filepath.Join(bootstrapRoot, entry.Root, entry.Relative)
		if seenEntries[destination] {
			return fmt.Errorf("sealed AppContainer bootstrap entry %q is duplicated", entry.Relative)
		}
		seenEntries[destination] = true
		if err := os.MkdirAll(destination, os.FileMode(entry.Mode)); err != nil {
			return err
		}
		if err := os.Chmod(destination, os.FileMode(entry.Mode)); err != nil {
			return err
		}
	}
	for _, entry := range manifest.Files {
		if !seenRoots[entry.Root] || !filepath.IsLocal(entry.Relative) || filepath.IsAbs(entry.Relative) ||
			entry.Bytes < 0 || len(entry.SHA256) != 64 || filepath.Base(entry.StagedLeaf) != entry.StagedLeaf {
			return fmt.Errorf("sealed AppContainer bootstrap entry %q is invalid", entry.Relative)
		}
		destination := filepath.Join(bootstrapRoot, entry.Root, entry.Relative)
		if seenEntries[destination] {
			return fmt.Errorf("sealed AppContainer bootstrap entry %q is duplicated", entry.Relative)
		}
		seenEntries[destination] = true
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		input, verifyInput, err := platformOpenProtectedOutput(rootAuthority, entry.StagedLeaf)
		if err != nil {
			return err
		}
		inputInfo, err := input.Stat()
		if err != nil || !inputInfo.Mode().IsRegular() || inputInfo.Size() != entry.Bytes {
			return errors.Join(errors.New("sealed AppContainer staged input identity is invalid"), err, input.Close())
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(entry.Mode))
		if err != nil {
			return errors.Join(err, input.Close())
		}
		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hasher), input)
		chmodErr := output.Chmod(os.FileMode(entry.Mode))
		closeFilesErr := errors.Join(output.Close(), verifyInput(), input.Close())
		observed := hex.EncodeToString(hasher.Sum(nil))
		if err := errors.Join(copyErr, chmodErr, closeFilesErr); err != nil ||
			written != entry.Bytes || observed != entry.SHA256 {
			return errors.Join(fmt.Errorf("sealed AppContainer staged input %q changed", entry.Relative), err)
		}
		if err := os.Remove(filepath.Join(configuration.PrivateRoot, entry.StagedLeaf)); err != nil {
			return err
		}
	}
	return os.Remove(configuration.BootstrapManifest)
}
