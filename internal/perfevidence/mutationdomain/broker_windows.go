//go:build windows

package mutationdomain

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	procThreadAttributeSecurityCapabilities = 0x00020009
	brokerProcessDescriptor                 = "D:P(D;;GA;;;OW)(A;;GA;;;SY)"
	brokerThreadDescriptor                  = "D:P(D;;GA;;;OW)(A;;GA;;;SY)"
	windowsStillActive                      = 259
	jobObjectBasicAccountingInformation     = 1
	windowsJobSettlementTimeout             = 5 * time.Second
	windowsJobSettlementPollInterval        = 10 * time.Millisecond
	windowsImageTeardownTimeout             = 5 * time.Second
	windowsImageTeardownPollInterval        = 10 * time.Millisecond
)

type windowsJobAccounting struct {
	totalUserTime             int64
	totalKernelTime           int64
	thisPeriodTotalUserTime   int64
	thisPeriodTotalKernelTime int64
	totalPageFaultCount       uint32
	totalProcesses            uint32
	activeProcesses           uint32
	totalTerminatedProcesses  uint32
}

type securityCapabilities struct {
	AppContainerSID *windows.SID
	Capabilities    *windows.SIDAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

var (
	userEnvironmentDLL        = windows.NewLazySystemDLL("userenv.dll")
	ntQueryObject             = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryObject")
	ntSetSecurityObject       = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtSetSecurityObject")
	createAppContainerProfile = userEnvironmentDLL.NewProc("CreateAppContainerProfile")
	deleteAppContainerProfile = userEnvironmentDLL.NewProc("DeleteAppContainerProfile")
	getAppContainerFolderPath = userEnvironmentDLL.NewProc("GetAppContainerFolderPath")
)

type windowsObjectBasicInformation struct {
	Attributes    uint32
	GrantedAccess uint32
	HandleCount   uint32
	PointerCount  uint32
	Reserved      [10]uint32
}

type windowsProcessSpec struct {
	executable        string
	arguments         []string
	environment       []string
	directory         string
	processDescriptor string
	threadDescriptor  string
	packageSID        *windows.SID
	capabilitySID     *windows.SID
	appContainer      bool
	token             windows.Token
	inherited         []windows.Handle
	ownJob            bool
	suspended         bool
}

type windowsProcessControl struct {
	mu       sync.Mutex
	handle   windows.Handle
	pid      uint32
	job      windows.Handle
	waitOnce sync.Once
	waitErr  error
}

type windowsStartedProcess struct {
	control    *windowsProcessControl
	stdin      *os.File
	stdout     *os.File
	stderr     *limitedBuffer
	thread     windows.Handle
	stderrDone <-chan error
	resumeOnce sync.Once
	resumeErr  error
	waitOnce   sync.Once
	waitErr    error
}

func createSealedWindowsProcess(spec windowsProcessSpec) (*windowsStartedProcess, error) {
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, stdinRead.Close(), stdinWrite.Close())
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, stdinRead.Close(), stdinWrite.Close(), stdoutRead.Close(), stdoutWrite.Close())
	}
	closeAll := func() error {
		return errors.Join(
			stdinRead.Close(), stdinWrite.Close(), stdoutRead.Close(), stdoutWrite.Close(),
			stderrRead.Close(), stderrWrite.Close(),
		)
	}
	childHandles := []windows.Handle{
		windows.Handle(stdinRead.Fd()), windows.Handle(stdoutWrite.Fd()), windows.Handle(stderrWrite.Fd()),
	}
	childHandles = append(childHandles, spec.inherited...)
	for _, handle := range childHandles {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return nil, errors.Join(err, closeAll())
		}
	}
	attributeCount := uint32(1)
	if spec.appContainer {
		if spec.packageSID == nil || spec.capabilitySID == nil {
			return nil, errors.Join(errors.New("AppContainer package or isolation capability SID is unavailable"), closeAll())
		}
		attributeCount++
	}
	attributes, err := windows.NewProcThreadAttributeList(attributeCount)
	if err != nil {
		return nil, errors.Join(err, closeAll())
	}
	defer attributes.Delete()
	if err := attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&childHandles[0]), uintptr(len(childHandles))*unsafe.Sizeof(childHandles[0]),
	); err != nil {
		return nil, errors.Join(err, closeAll())
	}
	var security securityCapabilities
	var privateCapability windows.SIDAndAttributes
	if spec.appContainer {
		security.AppContainerSID = spec.packageSID
		privateCapability = windows.SIDAndAttributes{Sid: spec.capabilitySID, Attributes: windows.SE_GROUP_ENABLED}
		security.Capabilities = &privateCapability
		security.CapabilityCount = 1
		if err := attributes.Update(
			procThreadAttributeSecurityCapabilities, unsafe.Pointer(&security), unsafe.Sizeof(security),
		); err != nil {
			return nil, errors.Join(fmt.Errorf("set AppContainer process attribute: %w", err), closeAll())
		}
	}
	executableName, err := windows.UTF16PtrFromString(spec.executable)
	if err != nil {
		return nil, errors.Join(err, closeAll())
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{spec.executable}, spec.arguments...)))
	if err != nil {
		return nil, errors.Join(err, closeAll())
	}
	directoryName, err := windows.UTF16PtrFromString(spec.directory)
	if err != nil {
		return nil, errors.Join(err, closeAll())
	}
	var environmentBlock []uint16
	var environmentPointer *uint16
	if spec.environment != nil {
		environmentBlock, err = windowsEnvironmentBlock(spec.environment)
		if err != nil {
			return nil, errors.Join(err, closeAll())
		}
		environmentPointer = &environmentBlock[0]
	}
	creationProcessDescriptor := spec.processDescriptor
	creationThreadDescriptor := spec.threadDescriptor
	if spec.appContainer {
		creationProcessDescriptor, err = hostProcessCreationDescriptor()
		if err != nil {
			return nil, errors.Join(err, closeAll())
		}
		creationThreadDescriptor = creationProcessDescriptor
	}
	processDescriptor, err := windows.SecurityDescriptorFromString(creationProcessDescriptor)
	if err != nil {
		return nil, errors.Join(err, closeAll())
	}
	threadDescriptor, err := windows.SecurityDescriptorFromString(creationThreadDescriptor)
	if err != nil {
		return nil, errors.Join(err, closeAll())
	}
	processAttributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: processDescriptor,
	}
	threadAttributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: threadDescriptor,
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES,
			StdInput: childHandles[0], StdOutput: childHandles[1], StdErr: childHandles[2],
		},
		ProcThreadAttributeList: attributes.List(),
	}
	var information windows.ProcessInformation
	creationFlags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW)
	if spec.suspended {
		creationFlags |= windows.CREATE_SUSPENDED
	}
	var createErr error
	if spec.token != 0 {
		createErr = windows.CreateProcessAsUser(
			spec.token, executableName, commandLine, &processAttributes, &threadAttributes, true,
			creationFlags, environmentPointer, directoryName, &startup.StartupInfo, &information,
		)
	} else {
		createErr = windows.CreateProcess(
			executableName, commandLine, &processAttributes, &threadAttributes, true,
			creationFlags, environmentPointer, directoryName, &startup.StartupInfo, &information,
		)
	}
	parentPipeCloseErr := errors.Join(stdinRead.Close(), stdoutWrite.Close(), stderrWrite.Close())
	if createErr != nil {
		if information.Thread != 0 {
			_ = windows.CloseHandle(information.Thread)
		}
		if information.Process != 0 {
			_ = windows.CloseHandle(information.Process)
		}
		return nil, errors.Join(fmt.Errorf("create sealed Windows process: %w", createErr), parentPipeCloseErr, stdinWrite.Close(), stdoutRead.Close(), stderrRead.Close())
	}
	if spec.appContainer {
		if !spec.suspended {
			_ = windows.TerminateProcess(information.Process, 1)
			return nil, errors.Join(
				errors.New("AppContainer process must remain suspended through ACL and token attestation"),
				windows.CloseHandle(information.Thread), windows.CloseHandle(information.Process), parentPipeCloseErr,
				stdinWrite.Close(), stdoutRead.Close(), stderrRead.Close(),
			)
		}
		if err := sealWindowsKernelHandleDACL(information.Process, spec.processDescriptor); err != nil {
			_ = windows.TerminateProcess(information.Process, 1)
			return nil, errors.Join(
				fmt.Errorf("seal suspended AppContainer process DACL: %w", err),
				windows.CloseHandle(information.Thread), windows.CloseHandle(information.Process), parentPipeCloseErr,
				stdinWrite.Close(), stdoutRead.Close(), stderrRead.Close(),
			)
		}
		if err := sealWindowsKernelHandleDACL(information.Thread, spec.threadDescriptor); err != nil {
			_ = windows.TerminateProcess(information.Process, 1)
			return nil, errors.Join(
				fmt.Errorf("seal suspended AppContainer thread DACL: %w", err),
				windows.CloseHandle(information.Thread), windows.CloseHandle(information.Process), parentPipeCloseErr,
				stdinWrite.Close(), stdoutRead.Close(), stderrRead.Close(),
			)
		}
	}
	control := &windowsProcessControl{handle: information.Process, pid: information.ProcessId}
	if spec.ownJob {
		job, jobErr := newKillOnCloseJob()
		if jobErr == nil {
			jobErr = windows.AssignProcessToJobObject(job, information.Process)
		}
		if jobErr != nil {
			_ = windows.TerminateProcess(information.Process, 1)
			return nil, errors.Join(
				jobErr, windows.CloseHandle(job), windows.CloseHandle(information.Thread),
				windows.CloseHandle(information.Process), parentPipeCloseErr,
				stdinWrite.Close(), stdoutRead.Close(), stderrRead.Close(),
			)
		}
		control.job = job
	}
	if spec.appContainer {
		if err := verifyWindowsLauncherReopenDenied(information.ProcessId); err != nil {
			_ = control.kill()
			return nil, errors.Join(
				err, windows.CloseHandle(information.Thread), control.wait(), parentPipeCloseErr,
				stdinWrite.Close(), stdoutRead.Close(), stderrRead.Close(),
			)
		}
	}
	if parentPipeCloseErr != nil {
		_ = control.kill()
		return nil, errors.Join(
			parentPipeCloseErr, windows.CloseHandle(information.Thread), control.wait(),
			stdinWrite.Close(), stdoutRead.Close(), stderrRead.Close(),
		)
	}
	thread := information.Thread
	if !spec.suspended {
		if err := windows.CloseHandle(thread); err != nil {
			_ = control.kill()
			return nil, errors.Join(err, control.wait(), stdinWrite.Close(), stdoutRead.Close(), stderrRead.Close())
		}
		thread = 0
	}
	stderrBuffer := &limitedBuffer{limit: maximumCapturedBytes}
	stderrDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stderrBuffer, stderrRead)
		stderrDone <- errors.Join(copyErr, stderrRead.Close())
	}()
	return &windowsStartedProcess{
		control: control, stdin: stdinWrite, stdout: stdoutRead, stderr: stderrBuffer,
		thread: thread, stderrDone: stderrDone,
	}, nil
}

func (process *windowsStartedProcess) resume() error {
	process.resumeOnce.Do(func() {
		if process.thread == 0 {
			return
		}
		if _, err := windows.ResumeThread(process.thread); err != nil {
			process.resumeErr = err
		}
		process.resumeErr = errors.Join(process.resumeErr, windows.CloseHandle(process.thread))
		process.thread = 0
	})
	return process.resumeErr
}

func (process *windowsStartedProcess) kill() error {
	if process == nil || process.control == nil {
		return nil
	}
	var threadErr error
	if process.thread != 0 {
		threadErr = windows.CloseHandle(process.thread)
		process.thread = 0
	}
	return errors.Join(threadErr, process.control.kill())
}

func (process *windowsStartedProcess) wait() error {
	process.waitOnce.Do(func() {
		controlErr := process.control.wait()
		stderrErr := <-process.stderrDone
		var diagnosticErr error
		if controlErr != nil {
			if diagnostic := strings.TrimSpace(string(process.stderr.snapshot())); diagnostic != "" {
				diagnosticErr = fmt.Errorf("isolated process stderr: %s", diagnostic)
			}
		}
		process.waitErr = errors.Join(controlErr, stderrErr, diagnosticErr)
	})
	return process.waitErr
}

func (process *windowsStartedProcess) closePipes() error {
	if process == nil {
		return nil
	}
	return errors.Join(process.stdin.Close(), process.stdout.Close())
}

func (process *windowsProcessControl) handleValue() windows.Handle {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.handle
}

func (process *windowsProcessControl) exitCode() (uint32, error) {
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.handle == 0 || process.handle == windows.InvalidHandle {
		return 0, errors.New("isolated process handle is closed")
	}
	var code uint32
	err := windows.GetExitCodeProcess(process.handle, &code)
	return code, err
}

func (process *windowsProcessControl) kill() error {
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.handle == 0 || process.handle == windows.InvalidHandle {
		return nil
	}
	if process.job != 0 {
		return windows.TerminateJobObject(process.job, 1)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.handle, &exitCode); err == nil && exitCode != windowsStillActive {
		return nil
	}
	return windows.TerminateProcess(process.handle, 1)
}

func (process *windowsProcessControl) wait() error {
	process.waitOnce.Do(func() {
		process.mu.Lock()
		handle := process.handle
		process.mu.Unlock()
		if handle == 0 || handle == windows.InvalidHandle {
			return
		}
		_, waitErr := windows.WaitForSingleObject(handle, windows.INFINITE)
		var exitCode uint32
		exitErr := windows.GetExitCodeProcess(handle, &exitCode)
		process.mu.Lock()
		job := process.job
		process.job = 0
		handleCloseErr := windows.CloseHandle(process.handle)
		process.handle = 0
		process.mu.Unlock()
		jobSettlementErr := settleWindowsJob(job)
		if exitErr == nil && exitCode != 0 {
			exitErr = fmt.Errorf("isolated process exited with code %d", exitCode)
		}
		process.waitErr = errors.Join(waitErr, exitErr, jobSettlementErr, handleCloseErr)
	})
	return process.waitErr
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	); err != nil {
		return 0, errors.Join(err, windows.CloseHandle(job))
	}
	return job, nil
}

func verifyWindowsLauncherReopenDenied(processID uint32) error {
	const launcherAuthority = windows.PROCESS_CREATE_PROCESS |
		windows.PROCESS_DUP_HANDLE |
		windows.PROCESS_VM_OPERATION |
		windows.PROCESS_VM_WRITE |
		windows.WRITE_DAC |
		windows.WRITE_OWNER
	handle, err := windows.OpenProcess(launcherAuthority, false, processID)
	if err == nil {
		return errors.Join(
			errors.New("sealed AppContainer process can be reopened as an external launcher"),
			windows.CloseHandle(handle),
		)
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("attest denial of external AppContainer launcher authority: %w", err)
	}
	return nil
}

func settleWindowsJob(job windows.Handle) error {
	if job == 0 || job == windows.InvalidHandle {
		return nil
	}
	active, queryErr := windowsJobActiveProcesses(job)
	var terminateErr error
	if queryErr == nil && active > 0 {
		terminateErr = windows.TerminateJobObject(job, 1)
	}
	deadline := time.Now().Add(windowsJobSettlementTimeout)
	for queryErr == nil && active > 0 && time.Now().Before(deadline) {
		time.Sleep(windowsJobSettlementPollInterval)
		active, queryErr = windowsJobActiveProcesses(job)
	}
	var settlementErr error
	if queryErr == nil && active != 0 {
		settlementErr = fmt.Errorf("Windows job retained %d active processes after cleanup", active)
	}
	return errors.Join(queryErr, terminateErr, settlementErr, windows.CloseHandle(job))
}

func windowsJobActiveProcesses(job windows.Handle) (uint32, error) {
	information := windowsJobAccounting{}
	err := windows.QueryInformationJobObject(
		job,
		jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
		nil,
	)
	return information.activeProcesses, err
}

func runWindowsBroker(parentInput io.Reader, parentOutput io.Writer, retainedImage *os.File) (resultErr error) {
	reader := bufio.NewReaderSize(parentInput, maximumProtocolLine)
	var configuration initialization
	if err := readJSONLine(reader, &configuration); err != nil {
		return fmt.Errorf("read broker initialization: %w", err)
	}
	authority, err := createPrivateAppContainer(configuration, retainedImage)
	if err != nil {
		return errors.Join(writeJSONLine(parentOutput, response{Error: err.Error(), ExitCode: -1}), err)
	}
	defer func() { resultErr = errors.Join(resultErr, authority.close()) }()
	configuration.PrivateRoot = authority.rootPath
	configuration.BootstrapManifest = authority.manifestPath
	started, err := createSealedWindowsProcess(windowsProcessSpec{
		executable:        authority.helperPath,
		arguments:         []string{helperArgument},
		directory:         os.Getenv("SystemRoot"),
		processDescriptor: appContainerProcessDescriptor(authority.traditionalSID, authority.capabilitySID),
		threadDescriptor:  appContainerThreadDescriptor(authority.traditionalSID, authority.capabilitySID),
		packageSID:        authority.packageSID,
		capabilitySID:     authority.capabilitySID,
		appContainer:      true,
		ownJob:            true,
		suspended:         true,
	})
	if err != nil {
		startErr := fmt.Errorf("start no-network AppContainer helper: %w", err)
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: startErr.Error(), ExitCode: -1}),
			startErr,
		)
	}
	if err := verifyWindowsProcessImage(
		started.control.handleValue(), authority.helper, authority.helperPath, false,
	); err != nil {
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: err.Error(), ExitCode: -1}),
			err, started.kill(), started.closePipes(), started.wait(),
		)
	}
	if err := verifyPrivateAppContainerProcess(started.control.handleValue(), appContainerIdentity{
		traditionalUserSID:     authority.traditionalSID,
		packageSID:             authority.packageSID,
		isolationCapabilitySID: authority.capabilitySID,
	}); err != nil {
		attestationErr := fmt.Errorf("attest suspended AppContainer helper: %w", err)
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: attestationErr.Error(), ExitCode: -1}),
			attestationErr, started.kill(), started.closePipes(), started.wait(),
		)
	}
	if err := started.resume(); err != nil {
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: err.Error(), ExitCode: -1}),
			err, started.kill(), started.closePipes(), started.wait(),
		)
	}
	if err := writeJSONLine(started.stdin, configuration); err != nil {
		return errors.Join(err, started.kill(), started.closePipes(), started.wait())
	}
	helperReader := bufio.NewReaderSize(started.stdout, maximumProtocolLine)
	var ready response
	if err := readJSONLine(helperReader, &ready); err != nil {
		exitCode, exitCodeErr := started.control.exitCode()
		settleErr := errors.Join(started.kill(), started.closePipes(), started.wait())
		stderr := strings.TrimSpace(string(started.stderr.snapshot()))
		startErr := errors.Join(
			fmt.Errorf("initialize no-network AppContainer helper: %w", err),
			fmt.Errorf("AppContainer helper exit code at protocol EOF: %d: %w", exitCode, exitCodeErr),
		)
		if stderr != "" {
			startErr = errors.Join(startErr, fmt.Errorf("AppContainer helper stderr: %s", stderr))
		}
		startErr = errors.Join(startErr, settleErr)
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: startErr.Error(), ExitCode: -1}),
			startErr,
		)
	}
	if ready.Error != "" {
		readyErr := errors.New(ready.Error)
		return errors.Join(
			writeJSONLine(parentOutput, ready), readyErr,
			started.kill(), started.closePipes(), started.wait(),
		)
	}
	if err := writeJSONLine(parentOutput, ready); err != nil {
		return errors.Join(err, started.kill(), started.closePipes(), started.wait())
	}
	inputDone := make(chan error, 1)
	outputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(started.stdin, reader)
		inputDone <- errors.Join(copyErr, started.stdin.Close())
	}()
	go func() {
		_, copyErr := io.Copy(parentOutput, helperReader)
		outputDone <- errors.Join(copyErr, started.stdout.Close())
	}()
	var proxyErr error
	select {
	case inputErr := <-inputDone:
		proxyErr = inputErr
		if inputErr != nil {
			proxyErr = errors.Join(proxyErr, started.kill())
		}
		proxyErr = errors.Join(proxyErr, <-outputDone)
	case outputErr := <-outputDone:
		proxyErr = outputErr
		proxyErr = errors.Join(proxyErr, started.stdin.Close())
	}
	return errors.Join(proxyErr, started.kill(), started.wait())
}

type appContainerAuthority struct {
	profileName     string
	profileMarker   string
	traditionalSID  *windows.SID
	packageSID      *windows.SID
	capabilitySID   *windows.SID
	descriptor      string
	rootPath        string
	root            windows.Handle
	helperPath      string
	helperLeaf      string
	helperDirectory windows.Handle
	helperSecurity  windows.Handle
	helper          *os.File
	manifestPath    string
}

type sealedObjectCreator struct {
	token           windows.Token
	descriptor      *windows.SECURITY_DESCRIPTOR
	finalDescriptor string
}

func createPrivateAppContainer(configuration initialization, retainedImage *os.File) (*appContainerAuthority, error) {
	profileEntropy, err := randomBytes(appContainerProfileEntropyBytes)
	if err != nil {
		return nil, err
	}
	profileName := appContainerProfilePrefix + hex.EncodeToString(profileEntropy)
	profileMarker, err := createAppContainerRecoveryMarker(configuration.RuntimeRoot, profileName)
	if err != nil {
		return nil, err
	}
	authority := &appContainerAuthority{profileName: profileName, profileMarker: profileMarker}
	fail := func(operationErr error) (*appContainerAuthority, error) {
		return nil, errors.Join(operationErr, authority.close())
	}
	packageSID, err := createEphemeralAppContainerProfile(profileName)
	if err != nil {
		return fail(err)
	}
	authority.packageSID = packageSID
	authority.traditionalSID, err = tokenUserSID(windows.GetCurrentProcessToken())
	if err != nil {
		return fail(fmt.Errorf("retain trusted Windows user SID: %w", err))
	}
	authority.capabilitySID, err = newIsolationCapabilitySID()
	if err != nil {
		return fail(fmt.Errorf("derive private AppContainer isolation capability: %w", err))
	}
	authority.descriptor = appContainerObjectDescriptor(authority.traditionalSID, authority.capabilitySID)
	creationDescriptorText, err := hostObjectCreationDescriptor()
	if err != nil {
		return fail(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(creationDescriptorText)
	if err != nil {
		return fail(err)
	}
	rootCreator := sealedObjectCreator{descriptor: descriptor}
	creator := sealedObjectCreator{descriptor: descriptor, finalDescriptor: authority.descriptor}
	rootEntropy, err := randomBytes(16)
	if err != nil {
		return fail(err)
	}
	profileRoot, err := appContainerFolderPath(authority.packageSID)
	if err != nil {
		return fail(err)
	}
	authority.rootPath = filepath.Join(
		profileRoot, privateRootDirectory+"-"+hex.EncodeToString(rootEntropy),
	)
	authority.root, err = rootCreator.create(0, windowsNTPath(authority.rootPath), true)
	if err != nil {
		return fail(fmt.Errorf("atomically create private AppContainer root: %w", err))
	}
	authority.manifestPath, err = stageSealedInputs(authority.rootPath, authority.root, configuration.Roots, creator)
	if err != nil {
		return fail(fmt.Errorf("stage sealed AppContainer inputs: %w", err))
	}
	if err := sealWindowsNamedDACL(authority.root, authority.descriptor); err != nil {
		return fail(fmt.Errorf("seal private AppContainer root DACL: %w", err))
	}
	if _, err := retainedImage.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	helperEntropy, err := randomBytes(16)
	if err != nil {
		return fail(err)
	}
	helperDirectoryPath := filepath.Join(profileRoot, "mutation-helper-"+hex.EncodeToString(helperEntropy))
	authority.helperDirectory, err = rootCreator.create(0, windowsNTPath(helperDirectoryPath), true)
	if err != nil {
		return fail(fmt.Errorf("create retained AppContainer helper directory: %w", err))
	}
	authority.helperLeaf = "helper.exe"
	authority.helperPath = filepath.Join(helperDirectoryPath, authority.helperLeaf)
	writableHelper, _, err := copySealedFile(
		retainedImage, authority.helperDirectory, authority.helperLeaf, rootCreator, true,
	)
	if err != nil {
		return fail(fmt.Errorf("create retained AppContainer helper image: %w", err))
	}
	authority.helper, authority.helperSecurity, err = finalizeWindowsHelperImage(
		writableHelper,
		authority.helperDirectory,
		authority.helperLeaf,
		authority.helperPath,
		authority.traditionalSID,
		authority.capabilitySID,
	)
	if err != nil {
		return fail(fmt.Errorf("seal retained AppContainer helper image: %w", err))
	}
	return authority, nil
}

func openSealedHelperImage(path string) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(windowsNTPath(path))
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	desired := uint32(
		windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA |
			windows.FILE_EXECUTE | windows.SYNCHRONIZE,
	)
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, desired, attributes, &status, nil, 0, windows.FILE_SHARE_READ,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	return handle, err
}

func appContainerFolderPath(packageSID *windows.SID) (string, error) {
	encodedSID, err := windows.UTF16PtrFromString(packageSID.String())
	if err != nil {
		return "", err
	}
	var folder *uint16
	result, _, _ := getAppContainerFolderPath.Call(
		uintptr(unsafe.Pointer(encodedSID)), uintptr(unsafe.Pointer(&folder)),
	)
	if int32(result) < 0 || folder == nil {
		return "", fmt.Errorf("resolve AppContainer storage: HRESULT 0x%08x", uint32(result))
	}
	path := windows.UTF16PtrToString(folder)
	windows.CoTaskMemFree(unsafe.Pointer(folder))
	return path, nil
}

func appContainerObjectDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	// Windows evaluates AppContainer access twice: the trusted invoking user must
	// pass the traditional check, and the fresh capability must independently pass
	// the restricted check. No ambient package or network capability is admitted.
	return fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)S:(ML;OICI;NW;;;LW)",
		traditionalUserSID.String(),
		capabilitySID.String(),
	)
}

func appContainerReadOnlyObjectDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	return fmt.Sprintf(
		"D:P(A;OICI;GRGX;;;%s)(A;OICI;GRGX;;;%s)(A;OICI;FA;;;SY)S:(ML;OICI;NW;;;LW)",
		traditionalUserSID.String(),
		capabilitySID.String(),
	)
}

func hostObjectCreationDescriptor() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)S:(ML;OICI;NW;;;LW)",
		user.User.Sid.String(),
	), nil
}

func appContainerHelperDescriptors(traditionalUserSID, capabilitySID *windows.SID) (file, directory string, err error) {
	trustedUserText := traditionalUserSID.String()
	capabilityText := capabilitySID.String()
	// The broker must be able to traverse and read the image because CreateProcess
	// opens it before constructing the AppContainer token. The trusted user and
	// private capability receive only read/execute; neither can rewrite its ACL.
	file = fmt.Sprintf(
		"D:P(D;;WDWO;;;OW)(A;;GRGX;;;%s)(A;;GRGX;;;%s)(A;;FA;;;SY)S:(ML;;NW;;;LW)",
		trustedUserText,
		capabilityText,
	)
	directory = fmt.Sprintf(
		"D:P(D;OICI;WDWO;;;OW)(A;OICI;GRGX;;;%s)(A;OICI;GRGX;;;%s)(A;OICI;FA;;;SY)S:(ML;OICI;NW;;;LW)",
		trustedUserText,
		capabilityText,
	)
	return file, directory, nil
}

func appContainerHelperTeardownDescriptor() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	// This descriptor is installed only after the helper Job reports zero active
	// processes and the broker has closed its pinned image handle. Avoiding
	// inheritable ACEs prevents teardown from rewriting the sealed child DACL.
	return fmt.Sprintf(
		"D:P(D;;WDWO;;;OW)(A;;GRGX;;;%s)(A;;DC;;;%s)(A;;FA;;;SY)S:(ML;;NW;;;LW)",
		user.User.Sid.String(),
		user.User.Sid.String(),
	), nil
}

func appContainerHelperFileTeardownDescriptor() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"D:P(A;;GRSD;;;%s)(A;;FA;;;SY)S:(ML;;NW;;;LW)",
		user.User.Sid.String(),
	), nil
}

func finalizeWindowsHelperImage(
	writable *os.File,
	directory windows.Handle,
	leaf string,
	path string,
	traditionalUserSID *windows.SID,
	capabilitySID *windows.SID,
) (*os.File, windows.Handle, error) {
	fileDescriptor, directoryDescriptor, err := appContainerHelperDescriptors(traditionalUserSID, capabilitySID)
	if err != nil {
		return nil, windows.InvalidHandle, errors.Join(err, writable.Close())
	}
	return finalizeWindowsExecutableFile(writable, directory, leaf, path, fileDescriptor, directoryDescriptor)
}

func finalizeWindowsExecutableFile(
	writable *os.File,
	directory windows.Handle,
	leaf string,
	path string,
	fileDescriptor string,
	directoryDescriptor string,
) (*os.File, windows.Handle, error) {
	securityAuthority, securityInformation, err := openWindowsFileSecurityAuthority(directory, leaf)
	if err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("retain executable security authority: %w", err), writable.Close())
	}
	var writableInformation windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(writable.Fd()), &writableInformation); err != nil ||
		!sameWindowsObject(writableInformation, securityInformation) {
		return nil, windows.InvalidHandle, errors.Join(
			errors.New("Windows executable security authority does not identify the copied image"),
			err,
			windows.CloseHandle(securityAuthority),
			writable.Close(),
		)
	}
	if err := sealWindowsHandleDACL(securityAuthority, fileDescriptor); err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("seal executable file DACL: %w", err), windows.CloseHandle(securityAuthority), writable.Close())
	}
	if err := sealWindowsHandleDACL(directory, directoryDescriptor); err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("seal executable directory DACL: %w", err), windows.CloseHandle(securityAuthority), writable.Close())
	}
	intermediate, intermediateInfo, err := openRetainedWindowsFile(
		directory,
		leaf,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	if err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("open intermediate executable read authority: %w", err), windows.CloseHandle(securityAuthority), writable.Close())
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(writable.Fd()), &writableInformation); err != nil ||
		!sameWindowsObject(writableInformation, intermediateInfo) {
		return nil, windows.InvalidHandle, errors.Join(
			errors.New("retained Windows executable identity changed before write authority was released"),
			err,
			windows.CloseHandle(intermediate),
			windows.CloseHandle(securityAuthority),
			writable.Close(),
		)
	}
	if err := writable.Close(); err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("release executable write authority: %w", err), windows.CloseHandle(intermediate), windows.CloseHandle(securityAuthority))
	}
	retained, retainedInfo, err := openRetainedWindowsFile(directory, leaf, windows.FILE_SHARE_READ)
	if err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("open final executable read authority: %w", err), windows.CloseHandle(intermediate), windows.CloseHandle(securityAuthority))
	}
	if !sameWindowsObject(intermediateInfo, retainedInfo) {
		return nil, windows.InvalidHandle, errors.Join(
			errors.New("retained Windows executable identity changed while sealing share access"),
			windows.CloseHandle(retained),
			windows.CloseHandle(intermediate),
			windows.CloseHandle(securityAuthority),
		)
	}
	if err := windows.CloseHandle(intermediate); err != nil {
		return nil, windows.InvalidHandle, errors.Join(err, windows.CloseHandle(retained), windows.CloseHandle(securityAuthority))
	}
	return os.NewFile(uintptr(retained), path), securityAuthority, nil
}

func openWindowsFileSecurityAuthority(
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
		windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
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
			errors.New("Windows file security authority is not a single-link no-follow regular file"),
			err,
			windows.CloseHandle(handle),
		)
	}
	return handle, information, nil
}

func openRetainedWindowsFile(
	root windows.Handle,
	leaf string,
	share uint32,
) (windows.Handle, windows.ByHandleFileInformation, error) {
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	desired := uint32(
		windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA |
			windows.FILE_EXECUTE | windows.SYNCHRONIZE,
	)
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		desired,
		attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		share,
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
			errors.New("retained helper image is not a single-link no-follow regular file"),
			err,
			windows.CloseHandle(handle),
		)
	}
	return handle, information, nil
}

func appContainerProcessDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	const processQueryAndSynchronize = "0x00121000"
	return fmt.Sprintf(
		"D:P(A;;%s;;;%s)(A;;%s;;;%s)(A;;GA;;;SY)",
		processQueryAndSynchronize,
		traditionalUserSID.String(),
		processQueryAndSynchronize,
		capabilitySID.String(),
	)
}

func appContainerThreadDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	const threadQueryAndSynchronize = "0x00120800"
	return fmt.Sprintf(
		"D:P(A;;%s;;;%s)(A;;%s;;;%s)(A;;GA;;;SY)",
		threadQueryAndSynchronize,
		traditionalUserSID.String(),
		threadQueryAndSynchronize,
		capabilitySID.String(),
	)
}

func hostProcessCreationDescriptor() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("D:P(A;;GA;;;%s)(A;;GA;;;SY)", user.User.Sid.String()), nil
}

func sealWindowsKernelHandleDACL(handle windows.Handle, descriptorText string) error {
	descriptor, err := windows.SecurityDescriptorFromString(descriptorText)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		handle,
		windows.SE_KERNEL_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

func sealWindowsFileDACL(file *os.File, descriptorText string) error {
	if file == nil {
		return errors.New("file DACL authority is unavailable")
	}
	return sealWindowsHandleDACL(windows.Handle(file.Fd()), descriptorText)
}

func sealWindowsHandleDACL(handle windows.Handle, descriptorText string) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return errors.New("DACL authority handle is unavailable")
	}
	descriptor, err := windows.SecurityDescriptorFromString(descriptorText)
	if err != nil {
		return err
	}
	status, _, _ := ntSetSecurityObject.Call(
		uintptr(handle),
		uintptr(windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION),
		uintptr(unsafe.Pointer(descriptor)),
	)
	if int32(status) < 0 {
		return windows.NTStatus(uint32(status))
	}
	return nil
}

func randomBytes(count int) ([]byte, error) {
	content := make([]byte, count)
	if _, err := rand.Read(content); err != nil {
		return nil, err
	}
	return content, nil
}

type tokenSecurityAttributeV1 struct {
	Name       windows.NTUnicodeString
	ValueType  uint16
	Reserved   uint16
	Flags      uint32
	ValueCount uint32
	Values     unsafe.Pointer
}

type tokenSecurityAttributesInformation struct {
	Version        uint16
	Reserved       uint16
	AttributeCount uint32
	Attributes     *tokenSecurityAttributeV1
}

type tokenAppContainerInformation struct {
	SID *windows.SID
}

type appContainerProcessClaim struct {
	Value0 uint64
	Value1 uint64
}

func verifyPrivateAppContainerProcess(process windows.Handle, identity appContainerIdentity) error {
	if process == 0 || process == windows.InvalidHandle {
		return errors.New("AppContainer process handle is unavailable")
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("open AppContainer process token: %w", err)
	}
	return errors.Join(verifyPrivateAppContainerToken(token, identity), token.Close())
}

func appContainerProcessClaimForProcess(process windows.Handle) (appContainerProcessClaim, error) {
	if process == 0 || process == windows.InvalidHandle {
		return appContainerProcessClaim{}, errors.New("AppContainer process handle is unavailable")
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return appContainerProcessClaim{}, err
	}
	claim, claimErr := appContainerProcessClaimForToken(token)
	return claim, errors.Join(claimErr, token.Close())
}

func verifyPrivateAppContainerToken(token windows.Token, identity appContainerIdentity) error {
	const (
		tokenIsAppContainer = 29
	)
	if identity.traditionalUserSID == nil || identity.packageSID == nil || identity.isolationCapabilitySID == nil {
		return errors.New("expected trusted user, AppContainer package, or isolation capability SID is unavailable")
	}
	isAppContainer, err := tokenUint32(token, tokenIsAppContainer)
	if err != nil || isAppContainer != 1 {
		return errors.Join(fmt.Errorf("token AppContainer marker is %d, want 1", isAppContainer), err)
	}
	observedPackageSID, err := appContainerSIDForToken(token)
	if err != nil {
		return fmt.Errorf("query token AppContainer SID: %w", err)
	}
	if !observedPackageSID.Equals(identity.packageSID) {
		return errors.New("token AppContainer SID does not match the ephemeral package")
	}
	observedUserSID, err := tokenUserSID(token)
	if err != nil {
		return fmt.Errorf("query AppContainer traditional user SID: %w", err)
	}
	if !observedUserSID.Equals(identity.traditionalUserSID) {
		return errors.New("AppContainer token does not retain the trusted invoking user")
	}
	capabilities, err := capabilitySIDsForToken(token)
	if err != nil {
		return fmt.Errorf("query private AppContainer capability: %w", err)
	}
	if len(capabilities) != 1 || !capabilities[0].Equals(identity.isolationCapabilitySID) {
		return fmt.Errorf("token capability set does not contain exactly the private isolation authority")
	}
	if err := verifyIsolationCapabilitySID(capabilities[0]); err != nil {
		return err
	}
	restrictions, err := tokenGroupCount(token, windows.TokenRestrictedSids)
	if err != nil || restrictions != 0 {
		return errors.Join(fmt.Errorf("token restriction SID count is %d, want 0", restrictions), err)
	}
	if err := verifyLowIntegrityToken(token); err != nil {
		return err
	}
	if err := verifyTokenHasNoEnabledPrivileges(token); err != nil {
		return err
	}
	attributes, err := tokenSecurityAttributeNames(token)
	if err != nil {
		return fmt.Errorf("query token security claims: %w", err)
	}
	// An ephemeral profile is an unpackaged AppContainer. Windows therefore
	// emits the process-unique claim but correctly does not forge the SYSAPPID
	// claim reserved for registered AppX/MSIX package identities.
	if !attributes["TSA://ProcUnique"] || attributes["WIN://SYSAPPID"] {
		names := make([]string, 0, len(attributes))
		for name := range attributes {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("unpackaged AppContainer claims are invalid (claims=%v)", names)
	}
	if _, err := appContainerProcessClaimForToken(token); err != nil {
		return err
	}
	return nil
}

func verifyLowIntegrityToken(token windows.Token) error {
	const securityMandatoryLowRID = 0x00001000
	buffer, err := tokenInformationBuffer(token, windows.TokenIntegrityLevel)
	if err != nil {
		return fmt.Errorf("query AppContainer integrity level: %w", err)
	}
	label := (*windows.SIDAndAttributes)(unsafe.Pointer(&buffer[0]))
	rid, err := sidLastSubAuthority(label.Sid)
	if err != nil {
		return fmt.Errorf("AppContainer integrity SID is invalid: %w", err)
	}
	if rid != securityMandatoryLowRID {
		return fmt.Errorf("AppContainer integrity RID is 0x%08x, want low integrity", rid)
	}
	return nil
}

func sidLastSubAuthority(sid *windows.SID) (uint32, error) {
	if sid == nil || !sid.IsValid() {
		return 0, errors.New("SID is invalid")
	}
	// x/sys obtains sub-authority pointers through uintptr-returning Windows APIs.
	// The canonical string keeps checkptr provenance intact for SIDs backed by a
	// Go-owned token-information buffer.
	value := sid.String()
	separator := strings.LastIndexByte(value, '-')
	if separator < 0 || separator == len(value)-1 {
		return 0, fmt.Errorf("SID %q has no terminal sub-authority", value)
	}
	parsed, err := strconv.ParseUint(value[separator+1:], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse terminal sub-authority from SID %q: %w", value, err)
	}
	return uint32(parsed), nil
}

func verifyTokenHasNoEnabledPrivileges(token windows.Token) error {
	name, err := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
	if err != nil {
		return err
	}
	var traversalPrivilege windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &traversalPrivilege); err != nil {
		return fmt.Errorf("resolve standard traversal privilege: %w", err)
	}
	buffer, err := tokenInformationBuffer(token, windows.TokenPrivileges)
	if err != nil {
		return fmt.Errorf("query AppContainer privileges: %w", err)
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&buffer[0]))
	for _, privilege := range privileges.AllPrivileges() {
		if privilege.Attributes&windows.SE_PRIVILEGE_ENABLED != 0 && privilege.Luid != traversalPrivilege {
			return fmt.Errorf(
				"AppContainer token retains enabled privilege LUID %d:%d",
				privilege.Luid.HighPart,
				privilege.Luid.LowPart,
			)
		}
	}
	// Windows keeps SeChangeNotifyPrivilege enabled even in a native AppContainer.
	// It only bypasses directory traverse checks; it cannot bypass the final
	// object DACL or the AppContainer restricted-token intersection.
	return nil
}

func appContainerSIDForToken(token windows.Token) (*windows.SID, error) {
	const tokenAppContainerSID = 31
	var required uint32
	err := windows.GetTokenInformation(token, tokenAppContainerSID, nil, 0, &required)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || required < uint32(unsafe.Sizeof(tokenAppContainerInformation{})) {
		return nil, err
	}
	words := make([]uint64, (required+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)[:required]
	if err := windows.GetTokenInformation(token, tokenAppContainerSID, &buffer[0], required, &required); err != nil {
		return nil, err
	}
	information := (*tokenAppContainerInformation)(unsafe.Pointer(&buffer[0]))
	if information.SID == nil {
		return nil, errors.New("token has no AppContainer package SID")
	}
	return information.SID.Copy()
}

func tokenUint32(token windows.Token, informationClass uint32) (uint32, error) {
	var value uint32
	var returned uint32
	err := windows.GetTokenInformation(
		token,
		informationClass,
		(*byte)(unsafe.Pointer(&value)),
		uint32(unsafe.Sizeof(value)),
		&returned,
	)
	return value, err
}

func tokenGroupCount(token windows.Token, informationClass uint32) (uint32, error) {
	buffer, err := tokenInformationBuffer(token, informationClass)
	if err != nil {
		return 0, err
	}
	return *(*uint32)(unsafe.Pointer(&buffer[0])), nil
}

func tokenInformationBuffer(token windows.Token, informationClass uint32) ([]byte, error) {
	var required uint32
	err := windows.GetTokenInformation(token, informationClass, nil, 0, &required)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || required < uint32(unsafe.Sizeof(uint32(0))) {
		return nil, err
	}
	words := make([]uint64, (required+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)[:required]
	if err := windows.GetTokenInformation(token, informationClass, &buffer[0], required, &required); err != nil {
		return nil, err
	}
	return buffer, nil
}

func tokenSecurityAttributeNames(token windows.Token) (map[string]bool, error) {
	buffer, err := tokenSecurityAttributes(token)
	if err != nil {
		return nil, err
	}
	information := (*tokenSecurityAttributesInformation)(unsafe.Pointer(&buffer[0]))
	if information.Version != 1 || information.AttributeCount > 1024 ||
		(information.AttributeCount > 0 && information.Attributes == nil) {
		return nil, errors.New("private AppContainer token security attributes are invalid")
	}
	result := make(map[string]bool, information.AttributeCount)
	for _, attribute := range unsafe.Slice(information.Attributes, information.AttributeCount) {
		if attribute.Name.Buffer == nil || attribute.Name.Length%2 != 0 {
			return nil, errors.New("private AppContainer token contains an invalid security attribute name")
		}
		name := windows.UTF16ToString(unsafe.Slice(attribute.Name.Buffer, attribute.Name.Length/2))
		result[name] = true
	}
	return result, nil
}

func appContainerProcessClaimForToken(token windows.Token) (appContainerProcessClaim, error) {
	const (
		claimSecurityAttributeTypeUint64     = 0x0002
		claimSecurityAttributeNonInheritable = 0x0001
		claimSecurityAttributeUnique         = 0x0040
	)
	buffer, err := tokenSecurityAttributes(token)
	if err != nil {
		return appContainerProcessClaim{}, err
	}
	information := (*tokenSecurityAttributesInformation)(unsafe.Pointer(&buffer[0]))
	if information.AttributeCount > 1024 || (information.AttributeCount > 0 && information.Attributes == nil) {
		return appContainerProcessClaim{}, errors.New("private AppContainer token security attributes are invalid")
	}
	for _, attribute := range unsafe.Slice(information.Attributes, information.AttributeCount) {
		if attribute.Name.Buffer == nil || attribute.Name.Length%2 != 0 {
			continue
		}
		name := windows.UTF16ToString(unsafe.Slice(attribute.Name.Buffer, attribute.Name.Length/2))
		if name != "TSA://ProcUnique" {
			continue
		}
		if attribute.ValueType != claimSecurityAttributeTypeUint64 || attribute.ValueCount != 2 ||
			attribute.Values == nil || attribute.Flags&(claimSecurityAttributeNonInheritable|claimSecurityAttributeUnique) !=
			claimSecurityAttributeNonInheritable|claimSecurityAttributeUnique {
			return appContainerProcessClaim{}, fmt.Errorf(
				"AppContainer process claim shape is type=%d flags=0x%x values=%d",
				attribute.ValueType,
				attribute.Flags,
				attribute.ValueCount,
			)
		}
		values := unsafe.Slice((*uint64)(attribute.Values), attribute.ValueCount)
		if values[0] == 0 || values[1] == 0 {
			return appContainerProcessClaim{}, errors.New("AppContainer process claim contains a zero identity component")
		}
		return appContainerProcessClaim{Value0: values[0], Value1: values[1]}, nil
	}
	return appContainerProcessClaim{}, errors.New("AppContainer process claim is unavailable")
}

func tokenSecurityAttributes(token windows.Token) ([]byte, error) {
	const tokenSecurityAttributes = 39
	var required uint32
	err := windows.GetTokenInformation(token, tokenSecurityAttributes, nil, 0, &required)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || required == 0 {
		return nil, err
	}
	words := make([]uint64, (required+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)[:required]
	if err := windows.GetTokenInformation(
		token, tokenSecurityAttributes, &buffer[0], required, &required,
	); err != nil {
		return nil, err
	}
	return buffer, nil
}

func (creator sealedObjectCreator) create(
	root windows.Handle,
	name string,
	directory bool,
) (windows.Handle, error) {
	if creator.token == 0 {
		handle, err := createSealedObject(root, name, directory, creator.descriptor)
		if err == nil && creator.finalDescriptor != "" {
			if sealErr := sealWindowsNamedDACL(handle, creator.finalDescriptor); sealErr != nil {
				err = fmt.Errorf("seal newly created object DACL: %w", sealErr)
			}
		}
		if err != nil && handle != 0 && handle != windows.InvalidHandle {
			return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(handle))
		}
		return handle, err
	}
	if err := windows.SetThreadToken(nil, creator.token); err != nil {
		return windows.InvalidHandle, err
	}
	handle, createErr := createSealedObject(root, name, directory, creator.descriptor)
	revertErr := windows.RevertToSelf()
	if createErr != nil && handle != windows.InvalidHandle {
		return windows.InvalidHandle, errors.Join(createErr, revertErr, windows.CloseHandle(handle))
	}
	return handle, errors.Join(createErr, revertErr)
}

func sealWindowsNamedDACL(handle windows.Handle, descriptorText string) error {
	path, err := finalWindowsHandlePath(handle)
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString(descriptorText)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("set named DACL revision %d: %w", *(*byte)(unsafe.Pointer(dacl)), err)
	}
	return nil
}

func createSealedObject(
	root windows.Handle,
	name string,
	directory bool,
	descriptor *windows.SECURITY_DESCRIPTOR,
) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	desired := uint32(
		windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
			windows.FILE_READ_EA | windows.FILE_WRITE_EA | windows.FILE_EXECUTE |
			windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.WRITE_DAC |
			windows.DELETE | windows.SYNCHRONIZE,
	)
	options := uint32(windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	share := uint32(windows.FILE_SHARE_READ)
	if directory {
		const (
			fileAddSubdirectory = 0x00000004
			fileDeleteChild     = 0x00000040
		)
		desired = windows.FILE_LIST_DIRECTORY | windows.FILE_WRITE_DATA | fileAddSubdirectory |
			windows.FILE_READ_EA | windows.FILE_WRITE_EA | windows.FILE_TRAVERSE | fileDeleteChild |
			windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.WRITE_DAC |
			windows.DELETE | windows.SYNCHRONIZE
		options = windows.FILE_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, desired, attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
		share, windows.FILE_CREATE, options, 0, 0,
	)
	return handle, err
}

func (authority *appContainerAuthority) close() error {
	if authority == nil {
		return nil
	}
	var errs []error
	if authority.helperSecurity != 0 && authority.helperSecurity != windows.InvalidHandle {
		teardownDescriptor, err := appContainerHelperFileTeardownDescriptor()
		if err == nil {
			err = sealWindowsHandleDACL(authority.helperSecurity, teardownDescriptor)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("enter retained helper image teardown phase: %w", err))
		}
	}
	if authority.helper != nil {
		errs = append(errs, authority.helper.Close())
		authority.helper = nil
	}
	if authority.helperDirectory != 0 && authority.helperDirectory != windows.InvalidHandle {
		teardownDescriptor, err := appContainerHelperTeardownDescriptor()
		granted, grantedErr := windowsHandleGrantedAccess(authority.helperDirectory)
		if err == nil {
			err = sealWindowsHandleDACL(authority.helperDirectory, teardownDescriptor)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"enter retained helper teardown phase (granted=0x%08x): %w",
				granted,
				errors.Join(err, grantedErr),
			))
		}
		if authority.helperLeaf != "" {
			err := removeWindowsImageAfterSettlement(authority.helperPath)
			if err != nil {
				errs = append(errs, fmt.Errorf("unlink retained helper image: %w", err))
			}
			authority.helperLeaf = ""
		}
		if authority.helperSecurity != 0 && authority.helperSecurity != windows.InvalidHandle {
			errs = append(errs, windows.CloseHandle(authority.helperSecurity))
			authority.helperSecurity = 0
		}
		if err := markWindowsHandleForDeletion(authority.helperDirectory); err != nil {
			errs = append(errs, fmt.Errorf("unlink retained helper directory: %w", err))
		}
		errs = append(errs, windows.CloseHandle(authority.helperDirectory))
		authority.helperDirectory = 0
	}
	if authority.helperSecurity != 0 && authority.helperSecurity != windows.InvalidHandle {
		errs = append(errs, windows.CloseHandle(authority.helperSecurity))
		authority.helperSecurity = 0
	}
	if authority.root != 0 && authority.root != windows.InvalidHandle {
		if err := markWindowsHandleForDeletion(authority.root); err != nil {
			errs = append(errs, fmt.Errorf("unlink private AppContainer root: %w", err))
		}
		errs = append(errs, windows.CloseHandle(authority.root))
		authority.root = 0
	}
	if authority.packageSID != nil {
		errs = append(errs, releaseNativeAppContainerSID(authority.packageSID))
		authority.packageSID = nil
	}
	authority.traditionalSID = nil
	authority.capabilitySID = nil
	if authority.profileName != "" {
		profileErr := deleteEphemeralAppContainerProfile(authority.profileName)
		if profileErr == nil && authority.profileMarker != "" {
			profileErr = os.Remove(authority.profileMarker)
			if errors.Is(profileErr, os.ErrNotExist) {
				profileErr = nil
			}
		}
		if profileErr == nil {
			authority.profileName = ""
			authority.profileMarker = ""
		}
		errs = append(errs, profileErr)
	}
	return errors.Join(errs...)
}

func removeWindowsImageAfterSettlement(path string) error {
	deadline := time.Now().Add(windowsImageTeardownTimeout)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("image section did not settle before teardown deadline: %w", err)
		}
		time.Sleep(windowsImageTeardownPollInterval)
	}
}

func windowsHandleGrantedAccess(handle windows.Handle) (uint32, error) {
	information := windowsObjectBasicInformation{}
	status, _, _ := ntQueryObject.Call(
		uintptr(handle),
		0,
		uintptr(unsafe.Pointer(&information)),
		unsafe.Sizeof(information),
		0,
	)
	if int32(status) < 0 {
		return 0, windows.NTStatus(uint32(status))
	}
	return information.GrantedAccess, nil
}

type retainedBrokerImage struct {
	path        string
	file        *os.File
	directories []windows.Handle
}

func createRetainedBrokerImage(_ string) (*retainedBrokerImage, error) {
	currentPath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	currentName, err := windows.UTF16PtrFromString(currentPath)
	if err != nil {
		return nil, err
	}
	currentHandle, err := windows.CreateFile(
		currentName, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return nil, err
	}
	image := &retainedBrokerImage{path: currentPath, file: os.NewFile(uintptr(currentHandle), currentPath)}
	fail := func(operationErr error) (*retainedBrokerImage, error) {
		return nil, errors.Join(operationErr, image.close())
	}
	for directory := filepath.Dir(currentPath); ; directory = filepath.Dir(directory) {
		encoded, err := windows.UTF16PtrFromString(directory)
		if err != nil {
			return fail(err)
		}
		handle, err := windows.CreateFile(
			encoded, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
		)
		if err != nil {
			return fail(fmt.Errorf("retain broker image ancestor %s: %w", directory, err))
		}
		image.directories = append(image.directories, handle)
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	finalPath, err := finalWindowsHandlePath(currentHandle)
	if err != nil {
		return fail(err)
	}
	if !strings.EqualFold(normalizeWindowsPath(finalPath), normalizeWindowsPath(currentPath)) {
		return fail(fmt.Errorf("broker image path changed while its ancestor authority was acquired"))
	}
	return image, nil
}

func (image *retainedBrokerImage) close() error {
	if image == nil || image.file == nil {
		return nil
	}
	var errs []error
	errs = append(errs, image.file.Close())
	image.file = nil
	for _, directory := range image.directories {
		errs = append(errs, windows.CloseHandle(directory))
	}
	image.directories = nil
	return errors.Join(errs...)
}

func markWindowsHandleForDeletion(handle windows.Handle) error {
	information := uint32(1)
	return windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	)
}

func openWindowsObjectForDeletion(
	root windows.Handle,
	leaf string,
	directory bool,
) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.DELETE|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		options,
		0,
		0,
	)
	return handle, err
}

func duplicateInheritableHandle(source windows.Handle) (windows.Handle, error) {
	var duplicate windows.Handle
	err := windows.DuplicateHandle(
		windows.CurrentProcess(), source, windows.CurrentProcess(), &duplicate,
		windows.GENERIC_READ, true, 0,
	)
	return duplicate, err
}

func verifyWindowsProcessImage(
	process windows.Handle,
	expected *os.File,
	expectedPath string,
	reopen bool,
) error {
	if process == 0 || process == windows.InvalidHandle || expected == nil {
		return errors.New("process image authority is unavailable")
	}
	buffer := make([]uint16, 32_768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return err
	}
	observedPath := windows.UTF16ToString(buffer[:size])
	if !strings.EqualFold(normalizeWindowsPath(observedPath), normalizeWindowsPath(expectedPath)) {
		return fmt.Errorf("launched image path %s does not match retained image %s", observedPath, expectedPath)
	}
	if !reopen {
		// The retained handle denies write and delete sharing, so equality of the
		// kernel-reported path binds the suspended process to that exact object.
		return nil
	}
	encoded, err := windows.UTF16PtrFromString(observedPath)
	if err != nil {
		return err
	}
	observed, err := windows.CreateFile(
		encoded, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return err
	}
	var expectedInfo, observedInfo windows.ByHandleFileInformation
	expectedErr := windows.GetFileInformationByHandle(windows.Handle(expected.Fd()), &expectedInfo)
	observedErr := windows.GetFileInformationByHandle(observed, &observedInfo)
	closeErr := windows.CloseHandle(observed)
	if err := errors.Join(expectedErr, observedErr, closeErr); err != nil {
		return err
	}
	if expectedInfo.VolumeSerialNumber != observedInfo.VolumeSerialNumber ||
		expectedInfo.FileIndexHigh != observedInfo.FileIndexHigh || expectedInfo.FileIndexLow != observedInfo.FileIndexLow {
		return errors.New("launched private mutation process image was substituted")
	}
	return nil
}

func finalWindowsHandlePath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 512)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}

func normalizeWindowsPath(path string) string {
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(clean, `\\?\UNC\`)
	}
	return strings.TrimPrefix(clean, `\\?\`)
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	entries := append([]string(nil), environment...)
	sort.Slice(entries, func(left, right int) bool {
		return strings.ToLower(entries[left]) < strings.ToLower(entries[right])
	})
	for _, entry := range entries {
		if strings.ContainsRune(entry, '\x00') || !strings.Contains(entry, "=") {
			return nil, fmt.Errorf("invalid Windows environment entry %q", entry)
		}
	}
	return utf16.Encode([]rune(strings.Join(entries, "\x00") + "\x00\x00")), nil
}

func windowsNTPath(path string) string {
	clean := filepath.Clean(path)
	switch {
	case strings.HasPrefix(clean, `\\?\UNC\`):
		return `\??\UNC\` + strings.TrimPrefix(clean, `\\?\UNC\`)
	case strings.HasPrefix(clean, `\\?\`):
		return `\??\` + strings.TrimPrefix(clean, `\\?\`)
	case strings.HasPrefix(clean, `\\`):
		return `\??\UNC\` + strings.TrimPrefix(clean, `\\`)
	default:
		return `\??\` + clean
	}
}
