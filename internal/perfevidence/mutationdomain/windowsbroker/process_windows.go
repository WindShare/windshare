//go:build windows

package windowsbroker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"

	mutationwire "github.com/windshare/windshare/internal/perfevidence/mutationdomain/wire"
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
	stderr     *mutationwire.BoundedCapture
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
	stderrBuffer := mutationwire.NewBoundedCapture(mutationwire.MaximumCapturedBytes)
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
			if diagnostic := strings.TrimSpace(string(process.stderr.Snapshot())); diagnostic != "" {
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
