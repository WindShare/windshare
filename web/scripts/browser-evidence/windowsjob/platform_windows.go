//go:build windows

package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	jobPollInterval                     = 20 * time.Millisecond
	maximumAuthenticatedExecutableBytes = 128 << 20
	// A hostile target must not make causal evidence consume unbounded handles
	// or memory. Larger trees fail closed instead of weakening attribution.
	maxTerminationSnapshotProcesses              = 4_096
	terminationSnapshotExitChurnAttempts         = 3
	initialJobProcessIDCapacity                  = 8
	maxWindowsProcessID                  uint64  = 1<<32 - 1
	privateExitCodePayloadMask           uint32  = (1 << 29) - 1
	windowsStillActiveExitCode           uint32  = 259
	compareStringEqual                   uintptr = 2
	launcherRootAcknowledgement          byte    = 0xa5
	deadlineExitCodeDomain                       = "windshare/windowsjob/deadline-exit/v1"
	parentExitCodeDomain                         = "windshare/windowsjob/parent-exit/v1"
	authorityExitCodeDomain                      = "windshare/windowsjob/authority-exit/v1"
)

// Job-information calls intentionally use LazyProc.Call: its uintptrescapes
// contract prevents race instrumentation or stack growth from invalidating the
// structure pointers that x/sys otherwise exposes through a uintptr wrapper.
var (
	kernel32DLL                   = windows.NewLazySystemDLL("kernel32.dll")
	isProcessInJobProcedure       = kernel32DLL.NewProc("IsProcessInJob")
	compareStringOrdinalProcedure = kernel32DLL.NewProc("CompareStringOrdinal")
	getHandleInformationProcedure = kernel32DLL.NewProc("GetHandleInformation")
	setJobInformationProcedure    = kernel32DLL.NewProc("SetInformationJobObject")
	queryJobInformationProcedure  = kernel32DLL.NewProc("QueryInformationJobObject")
)

type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type jobObjectBasicProcessIDListHeader struct {
	numberOfAssignedProcesses uint32
	numberOfProcessIDsInList  uint32
}

type managedJob struct {
	handle           windows.Handle
	terminationCodes terminationExitCodes
}

type terminationExitCodes struct {
	deadline  uint32
	parent    uint32
	authority uint32
}

type jobProcessAccounting struct {
	total  uint32
	active uint32
}

// jobLifecycleAuthority is consumed only after launcher identity is fenced out
// of the Job. Termination remains provisional until exact retained member exits
// and the process-generation counter jointly authenticate its cause.
type jobLifecycleAuthority interface {
	activeProcessCount() (uint32, error)
	captureTerminationSnapshot(root rootLifecycleAuthority, maximumProcesses int) (targetMemberSnapshot, error)
	processAccounting() (jobProcessAccounting, error)
	exitCodes() terminationExitCodes
	terminate(uint32) error
}

type rootLifecycleAuthority interface {
	processID() uint32
	retainExitAuthority() (processExitAuthority, error)
}

type managedRoot struct {
	handle windows.Handle
	pid    uint32
}

type processExitAuthority interface {
	processID() uint32
	verifyJobMembership(windows.Handle) error
	exactExitCode(time.Duration) (uint32, error)
	close()
}

type managedProcessExitAuthority struct {
	handle windows.Handle
	pid    uint32
}

type targetMemberSnapshot struct {
	totalProcessesBefore uint32
	members              []processExitAuthority
}

type terminationIntervention struct {
	applied  bool
	exitCode uint32
	snapshot targetMemberSnapshot
	reason   string
	timedOut bool
}

type assignedLauncher struct {
	eventReader      *os.File
	input            io.WriteCloser
	process          *os.Process
	membershipHandle windows.Handle
	wait             <-chan error
}

type launcherEventResult struct {
	event launcherEvent
	err   error
}

type controlResult struct {
	request terminateRequest
	err     error
}

type rootExitResult struct {
	status rootStatus
	err    error
}

type launcherHandoffResult struct {
	control         *controlResult
	deadlineArrived bool
}

func runSupervisorPlatform(
	request startRequest,
	statusPath string,
	controlPath string,
	rawInput io.Reader,
) error {
	if err := validateWindowsEnvironment(request.Environment); err != nil {
		return err
	}
	terminationCodes, err := deriveTerminationExitCodes(request.Nonce)
	if err != nil {
		return err
	}
	if err := ensureFreshStatusDestination(statusPath); err != nil {
		return err
	}
	if err := ensureFreshControlDestination(controlPath); err != nil {
		return err
	}
	job, err := createManagedJob()
	if err != nil {
		return err
	}
	job.terminationCodes = terminationCodes
	defer job.close()

	deadline := time.NewTimer(time.Duration(request.DeadlineMS) * time.Millisecond)
	defer deadline.Stop()
	parentControls, closeParentAuthority, err := watchParentProcess(request)
	if err != nil {
		return err
	}
	defer closeParentAuthority()
	fileControls, closeFileAuthority := watchTerminationControl(controlPath, request)
	defer closeFileAuthority()
	controls, closeControlMerge := mergeControlAuthorities(parentControls, fileControls)
	defer closeControlMerge()
	var rawInputFile *os.File
	if request.Stdin == nil {
		if err := requireExactRawEOF(rawInput); err != nil {
			return err
		}
	} else {
		var ok bool
		rawInputFile, ok = rawInput.(*os.File)
		if !ok {
			return errors.New("raw stdin authority must be an inherited anonymous file handle")
		}
	}
	launcher, err := startAssignedLauncher(job, request, rawInputFile)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(launcher.membershipHandle)
	defer launcher.eventReader.Close()
	defer launcher.input.Close()
	launcherEventChannel := make(chan launcherEventResult, 1)
	go func() {
		event, eventErr := readLauncherEvent(launcher.eventReader)
		launcherEventChannel <- launcherEventResult{event: event, err: eventErr}
	}()

	for {
		select {
		case eventResult := <-launcherEventChannel:
			if eventResult.err != nil {
				return terminateAfterAuthorityFailure(job, request, eventResult.err)
			}
			return superviseLaunchedTree(job, request, statusPath, eventResult.event, deadline, controls, launcher)
		case control := <-controls:
			if control.err != nil {
				return terminateAfterAuthorityFailure(job, request, control.err)
			}
			select {
			case eventResult := <-launcherEventChannel:
				if eventResult.err != nil {
					return terminateAfterAuthorityFailure(job, request, eventResult.err)
				}
				replayedControl := make(chan controlResult, 1)
				replayedControl <- control
				return superviseLaunchedTree(job, request, statusPath, eventResult.event, deadline, replayedControl, launcher)
			default:
			}
			return terminatePendingLaunch(job, request, statusPath, terminateReasonParentRequest, false, launcherEventChannel, launcher.wait)
		case <-deadline.C:
			select {
			case eventResult := <-launcherEventChannel:
				if eventResult.err != nil {
					return terminateAfterAuthorityFailure(job, request, eventResult.err)
				}
				immediateDeadline := time.NewTimer(0)
				defer immediateDeadline.Stop()
				return superviseLaunchedTree(job, request, statusPath, eventResult.event, immediateDeadline, controls, launcher)
			default:
			}
			return terminatePendingLaunch(job, request, statusPath, terminationReasonDeadline, true, launcherEventChannel, launcher.wait)
		}
	}
}

func runLauncherPlatform(
	request startRequest,
	eventHandleValue uintptr,
	stdinHandleValue uintptr,
	acknowledgementReader io.Reader,
) error {
	eventHandle := windows.Handle(eventHandleValue)
	if err := windows.SetHandleInformation(eventHandle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fmt.Errorf("make launcher event handle private: %w", err)
	}
	eventWriter := os.NewFile(eventHandleValue, "windowsjob-launcher-event")
	if eventWriter == nil {
		return errors.New("launcher event handle is invalid")
	}
	defer eventWriter.Close()

	stdin, err := readExactRawStdin(stdinHandleValue, request.Stdin)
	if err != nil {
		return fmt.Errorf("read exact raw target stdin: %w", err)
	}
	defer func() {
		for index := range stdin {
			stdin[index] = 0
		}
	}()
	executableLock, err := openAuthenticatedExecutable(request.Executable, request.ExecutableSHA256)
	if err != nil {
		return fmt.Errorf("authenticate target executable: %w", err)
	}
	if executableLock != nil {
		defer executableLock.Close()
	}
	command := exec.Command(request.Executable, request.Arguments...)
	command.Dir = request.CWD
	command.Env = environmentStrings(request.Environment)
	var targetInputReader *os.File
	var targetInputWriter *os.File
	if stdin != nil {
		targetInputReader, targetInputWriter, err = os.Pipe()
		if err != nil {
			return fmt.Errorf("create exact target stdin pipe: %w", err)
		}
		defer targetInputReader.Close()
		defer targetInputWriter.Close()
		command.Stdin = targetInputReader
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		failure := boundedDiagnostic(err)
		return writeCanonicalFrame(eventWriter, launcherEvent{
			SchemaVersion: protocolSchemaVersion,
			Type:          launcherEventSpawnFailed,
			PID:           0,
			ProcessHandle: 0,
			SpawnFailure:  &failure,
		})
	}
	if targetInputReader != nil {
		_ = targetInputReader.Close()
		if err := writeAll(targetInputWriter, stdin); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return fmt.Errorf("deliver exact target stdin: %w", err)
		}
		if err := targetInputWriter.Close(); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return fmt.Errorf("close exact target stdin: %w", err)
		}
		for index := range stdin {
			stdin[index] = 0
		}
	}
	var transferHandle windows.Handle
	var duplicateErr error
	if err := command.Process.WithHandle(func(handle uintptr) {
		duplicateErr = windows.DuplicateHandle(
			windows.CurrentProcess(),
			windows.Handle(handle),
			windows.CurrentProcess(),
			&transferHandle,
			0,
			false,
			windows.DUPLICATE_SAME_ACCESS,
		)
	}); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("read launcher-local root handle: %w", err)
	}
	if duplicateErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("retain launcher-local root handle: %w", duplicateErr)
	}
	if transferHandle == 0 || transferHandle == windows.InvalidHandle {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.New("retained launcher-local root handle is invalid")
	}
	defer func() {
		if transferHandle != 0 {
			_ = windows.CloseHandle(transferHandle)
		}
	}()
	if err := writeCanonicalFrame(eventWriter, launcherEvent{
		SchemaVersion: protocolSchemaVersion,
		Type:          launcherEventRootStarted,
		PID:           uint32(command.Process.Pid),
		ProcessHandle: uint64(uintptr(transferHandle)),
		SpawnFailure:  nil,
	}); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("report root handle: %w", err)
	}
	if err := eventWriter.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("close launcher event channel: %w", err)
	}
	acknowledgement := []byte{0}
	if _, err := io.ReadFull(acknowledgementReader, acknowledgement); err != nil || acknowledgement[0] != launcherRootAcknowledgement {
		_ = command.Process.Kill()
		_ = command.Wait()
		if err != nil {
			return fmt.Errorf("receive root-handle acknowledgement: %w", err)
		}
		return errors.New("root-handle acknowledgement is invalid")
	}
	if err := windows.CloseHandle(transferHandle); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("release launcher-local transfer handle: %w", err)
	}
	transferHandle = 0
	// The supervisor now owns the durable root handle. Releasing the launcher's
	// local process reference lets the trusted launcher leave the Job before its
	// accounting is interpreted as target-tree liveness.
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release launcher-local root process reference: %w", err)
	}
	return nil
}

func openAuthenticatedExecutable(path, expectedSHA256 string) (*os.File, error) {
	if expectedSHA256 == "" {
		return nil, nil
	}
	encodedPath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		encodedPath,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("wrap authenticated executable handle")
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return nil, errors.Join(errors.New("authenticated executable is not a regular file"), file.Close())
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maximumAuthenticatedExecutableBytes+1))
	if err != nil || written < 1 || written > maximumAuthenticatedExecutableBytes {
		return nil, errors.Join(errors.New("authenticated executable size is outside authority"), err, file.Close())
	}
	if hex.EncodeToString(digest.Sum(nil)) != expectedSHA256 {
		return nil, errors.Join(errors.New("authenticated executable digest differs"), file.Close())
	}
	return file, nil
}

func createManagedJob() (managedJob, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return managedJob{}, fmt.Errorf("create Job Object: %w", err)
	}
	job := managedJob{handle: handle}
	configured := false
	defer func() {
		if !configured {
			job.close()
		}
	}()
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return managedJob{}, fmt.Errorf("make Job Object handle non-inheritable: %w", err)
	}
	if err := verifyJobHandleNonInheritable(handle); err != nil {
		return managedJob{}, err
	}
	limits := &windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	result, _, callErr := setJobInformationProcedure.Call(
		uintptr(handle),
		uintptr(windows.JobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(limits)),
		unsafe.Sizeof(*limits),
	)
	runtime.KeepAlive(limits)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = syscall.EINVAL
		}
		return managedJob{}, fmt.Errorf("set Job Object limits: %w", callErr)
	}
	if err := verifyExactJobLimits(handle); err != nil {
		return managedJob{}, err
	}
	configured = true
	return job, nil
}

func deriveTerminationExitCodes(nonce string) (terminationExitCodes, error) {
	key, err := hex.DecodeString(nonce)
	if err != nil || len(key) != nonceEncodedBytes/2 {
		return terminationExitCodes{}, errors.New("derive termination exit codes from invalid nonce")
	}
	used := make(map[uint32]struct{}, 3)
	return terminationExitCodes{
		deadline:  derivePrivateExitCode(key, deadlineExitCodeDomain, used),
		parent:    derivePrivateExitCode(key, parentExitCodeDomain, used),
		authority: derivePrivateExitCode(key, authorityExitCodeDomain, used),
	}, nil
}

func derivePrivateExitCode(key []byte, domain string, used map[uint32]struct{}) uint32 {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain))
	digest := mac.Sum(nil)
	payload := binary.BigEndian.Uint32(digest[:4]) & privateExitCodePayloadMask
	for {
		candidate := uint32(windows.APPLICATION_ERROR) | payload
		if candidate != 0 && candidate != windowsStillActiveExitCode {
			if _, exists := used[candidate]; !exists {
				used[candidate] = struct{}{}
				return candidate
			}
		}
		payload = (payload + 1) & privateExitCodePayloadMask
	}
}

func verifyExactJobLimits(handle windows.Handle) error {
	limits := &windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	result, _, callErr := queryJobInformationProcedure.Call(
		uintptr(handle),
		uintptr(windows.JobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(limits)),
		unsafe.Sizeof(*limits),
		0,
	)
	runtime.KeepAlive(limits)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = syscall.EINVAL
		}
		return fmt.Errorf("read back Job Object limits: %w", callErr)
	}
	if limits.BasicLimitInformation.LimitFlags != windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE {
		return fmt.Errorf("Job Object limits read back as %#x, expected exact non-breakaway kill-on-close %#x",
			limits.BasicLimitInformation.LimitFlags,
			windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		)
	}
	return nil
}

func (job managedJob) close() {
	if job.handle != 0 && job.handle != windows.InvalidHandle {
		_ = windows.CloseHandle(job.handle)
	}
}

func (job managedJob) exitCodes() terminationExitCodes {
	return job.terminationCodes
}

func (job managedJob) activeProcessCount() (uint32, error) {
	accounting, err := job.processAccounting()
	if err != nil {
		return 0, err
	}
	return accounting.active, nil
}

func (job managedJob) processAccounting() (jobProcessAccounting, error) {
	accounting := &jobObjectBasicAccountingInformation{}
	result, _, callErr := queryJobInformationProcedure.Call(
		uintptr(job.handle),
		uintptr(windows.JobObjectBasicAccountingInformation),
		uintptr(unsafe.Pointer(accounting)),
		unsafe.Sizeof(*accounting),
		0,
	)
	runtime.KeepAlive(accounting)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = syscall.EINVAL
		}
		return jobProcessAccounting{}, fmt.Errorf("query Job Object process accounting: %w", callErr)
	}
	return jobProcessAccounting{
		total:  accounting.TotalProcesses,
		active: accounting.ActiveProcesses,
	}, nil
}

func (job managedJob) activeProcessIDs(maximumProcesses int) ([]uint32, error) {
	if maximumProcesses <= 0 {
		return nil, errors.New("Job process snapshot limit must be positive")
	}
	capacity := initialJobProcessIDCapacity
	if capacity > maximumProcesses {
		capacity = maximumProcesses
	}
	headerWords := int((unsafe.Sizeof(jobObjectBasicProcessIDListHeader{}) + unsafe.Sizeof(uintptr(0)) - 1) / unsafe.Sizeof(uintptr(0)))
	for {
		buffer := make([]uintptr, headerWords+capacity)
		header := (*jobObjectBasicProcessIDListHeader)(unsafe.Pointer(&buffer[0]))
		var returnedLength uint32
		result, _, callErr := queryJobInformationProcedure.Call(
			uintptr(job.handle),
			uintptr(windows.JobObjectBasicProcessIdList),
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(len(buffer))*unsafe.Sizeof(uintptr(0)),
			uintptr(unsafe.Pointer(&returnedLength)),
		)
		assigned := uint64(header.numberOfAssignedProcesses)
		listed := uint64(header.numberOfProcessIDsInList)
		if assigned > uint64(maximumProcesses) || listed > uint64(maximumProcesses) {
			runtime.KeepAlive(buffer)
			return nil, fmt.Errorf("Job contains more than the termination evidence limit of %d processes", maximumProcesses)
		}
		if result == 0 {
			runtime.KeepAlive(buffer)
			if errors.Is(callErr, windows.ERROR_MORE_DATA) {
				nextCapacity := int(assigned)
				if nextCapacity <= capacity {
					nextCapacity = capacity * 2
				}
				if nextCapacity > maximumProcesses {
					return nil, fmt.Errorf("Job contains more than the termination evidence limit of %d processes", maximumProcesses)
				}
				capacity = nextCapacity
				continue
			}
			if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
				callErr = syscall.EINVAL
			}
			return nil, fmt.Errorf("query Job Object process IDs: %w", callErr)
		}
		if listed > uint64(capacity) || listed != assigned {
			runtime.KeepAlive(buffer)
			return nil, errors.New("Job Object returned an incomplete process ID snapshot")
		}
		ids := make([]uint32, int(listed))
		for index := range ids {
			word := buffer[headerWords+index]
			if uint64(word) == 0 || uint64(word) > maxWindowsProcessID {
				runtime.KeepAlive(buffer)
				return nil, errors.New("Job Object returned an invalid process ID")
			}
			ids[index] = uint32(word)
		}
		runtime.KeepAlive(buffer)
		return ids, nil
	}
}

func (job managedJob) captureTerminationSnapshot(
	root rootLifecycleAuthority,
	maximumProcesses int,
) (targetMemberSnapshot, error) {
	accounting, err := job.processAccounting()
	if err != nil {
		return targetMemberSnapshot{}, err
	}
	for attempt := 0; attempt < terminationSnapshotExitChurnAttempts; attempt++ {
		snapshot, retry, err := job.captureTerminationSnapshotAttempt(
			root,
			maximumProcesses,
			accounting.total,
		)
		if err != nil {
			return targetMemberSnapshot{}, err
		}
		if !retry {
			return snapshot, nil
		}
	}
	return targetMemberSnapshot{}, errors.New("target process snapshot did not stabilize after bounded natural-exit retries")
}

func (job managedJob) captureTerminationSnapshotAttempt(
	root rootLifecycleAuthority,
	maximumProcesses int,
	totalProcessesBefore uint32,
) (snapshot targetMemberSnapshot, retry bool, resultErr error) {
	snapshot.totalProcessesBefore = totalProcessesBefore
	processIDs, err := job.activeProcessIDs(maximumProcesses)
	if err != nil {
		return targetMemberSnapshot{}, false, err
	}
	if len(processIDs) == 0 {
		confirmedProcessIDs, err := job.activeProcessIDs(maximumProcesses)
		if err != nil {
			return targetMemberSnapshot{}, false, err
		}
		if len(confirmedProcessIDs) != 0 {
			return targetMemberSnapshot{}, false, errors.New("target process membership appeared during empty snapshot confirmation")
		}
		return snapshot, false, nil
	}
	if uint64(len(processIDs)) > uint64(totalProcessesBefore) {
		return targetMemberSnapshot{}, false, errors.New("Job process snapshot exceeds its total-process generation")
	}
	defer func() {
		if resultErr != nil || retry {
			snapshot.close()
		}
	}()
	retained := make(map[uint32]struct{}, len(processIDs))
	initialMembers := make(map[uint32]struct{}, len(processIDs))
	for _, processID := range processIDs {
		initialMembers[processID] = struct{}{}
	}
	for _, processID := range processIDs {
		if _, duplicate := retained[processID]; duplicate {
			resultErr = errors.New("Job process snapshot contains a duplicate process ID")
			return
		}
		var authority processExitAuthority
		if processID == root.processID() {
			authority, err = root.retainExitAuthority()
		} else {
			authority, err = openProcessExitAuthority(processID)
		}
		if err != nil {
			cause := fmt.Errorf("retain exact exit authority for process %d: %w", processID, err)
			retry, resultErr = job.classifyBenignSnapshotLoss(
				processID,
				initialMembers,
				maximumProcesses,
				totalProcessesBefore,
				cause,
			)
			return
		}
		if authority.processID() != processID {
			authority.close()
			resultErr = errors.New("retained process identity changed during termination snapshot")
			return
		}
		if err := authority.verifyJobMembership(job.handle); err != nil {
			authority.close()
			cause := fmt.Errorf("verify retained process %d: %w", processID, err)
			retry, resultErr = job.classifyBenignSnapshotLoss(
				processID,
				initialMembers,
				maximumProcesses,
				totalProcessesBefore,
				cause,
			)
			return
		}
		snapshot.members = append(snapshot.members, authority)
		retained[processID] = struct{}{}
	}
	currentProcessIDs, err := job.activeProcessIDs(maximumProcesses)
	if err != nil {
		resultErr = err
		return
	}
	if len(currentProcessIDs) == 0 {
		snapshot.close()
		snapshot = targetMemberSnapshot{totalProcessesBefore: totalProcessesBefore}
		return
	}
	for _, processID := range currentProcessIDs {
		if _, captured := retained[processID]; !captured {
			resultErr = errors.New("target process membership changed during termination snapshot")
			return
		}
	}
	return
}

func (job managedJob) classifyBenignSnapshotLoss(
	lostProcessID uint32,
	initialMembers map[uint32]struct{},
	maximumProcesses int,
	totalProcessesBefore uint32,
	cause error,
) (bool, error) {
	accounting, err := job.processAccounting()
	if err != nil {
		return false, err
	}
	currentProcessIDs, err := job.activeProcessIDs(maximumProcesses)
	if err != nil {
		return false, err
	}
	return classifyBenignSnapshotLossEvidence(
		lostProcessID,
		initialMembers,
		totalProcessesBefore,
		accounting.total,
		currentProcessIDs,
		cause,
	)
}

func classifyBenignSnapshotLossEvidence(
	lostProcessID uint32,
	initialMembers map[uint32]struct{},
	totalProcessesBefore uint32,
	currentTotalProcesses uint32,
	currentProcessIDs []uint32,
	cause error,
) (bool, error) {
	if currentTotalProcesses != totalProcessesBefore {
		return false, errors.New("target process generation changed while retaining termination evidence")
	}
	for _, processID := range currentProcessIDs {
		if processID == lostProcessID {
			return false, cause
		}
		if _, existed := initialMembers[processID]; !existed {
			return false, errors.New("target process membership changed while retaining termination evidence")
		}
	}
	return true, nil
}

func openProcessExitAuthority(processID uint32) (processExitAuthority, error) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false,
		processID,
	)
	if err != nil {
		return nil, err
	}
	authority := &managedProcessExitAuthority{handle: handle, pid: processID}
	actualProcessID, err := windows.GetProcessId(handle)
	if err != nil {
		authority.close()
		return nil, err
	}
	if actualProcessID != processID {
		authority.close()
		return nil, errors.New("opened process identity does not match the requested PID")
	}
	return authority, nil
}

func (snapshot *targetMemberSnapshot) close() {
	for _, member := range snapshot.members {
		member.close()
	}
	snapshot.members = nil
}

func (authority *managedProcessExitAuthority) processID() uint32 {
	return authority.pid
}

func (authority *managedProcessExitAuthority) verifyJobMembership(job windows.Handle) error {
	member, err := isProcessInJob(authority.handle, job)
	if err != nil {
		return err
	}
	if !member {
		return errors.New("retained process is no longer a member of the managed Job Object")
	}
	return nil
}

func (authority *managedProcessExitAuthority) exactExitCode(maximumWait time.Duration) (uint32, error) {
	var waitMilliseconds uint64
	if maximumWait > 0 {
		waitMilliseconds = uint64(maximumWait / time.Millisecond)
		if maximumWait%time.Millisecond != 0 {
			waitMilliseconds++
		}
	}
	if waitMilliseconds >= uint64(windows.INFINITE) {
		waitMilliseconds = uint64(windows.INFINITE) - 1
	}
	result, err := windows.WaitForSingleObject(authority.handle, uint32(waitMilliseconds))
	if err != nil {
		return 0, fmt.Errorf("wait for retained process %d: %w", authority.pid, err)
	}
	if result != windows.WAIT_OBJECT_0 {
		return 0, fmt.Errorf("retained process %d was not signaled after Job completion", authority.pid)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(authority.handle, &exitCode); err != nil {
		return 0, fmt.Errorf("read retained process %d exit code: %w", authority.pid, err)
	}
	if exitCode == windowsStillActiveExitCode {
		return 0, fmt.Errorf("retained process %d reported STILL_ACTIVE after Job completion", authority.pid)
	}
	return exitCode, nil
}

func (authority *managedProcessExitAuthority) close() {
	if authority.handle != 0 && authority.handle != windows.InvalidHandle {
		_ = windows.CloseHandle(authority.handle)
		authority.handle = 0
	}
}

func (job managedJob) terminate(exitCode uint32) error {
	if err := windows.TerminateJobObject(job.handle, exitCode); err != nil {
		return fmt.Errorf("terminate Job Object: %w", err)
	}
	return nil
}

func startAssignedLauncher(
	job managedJob,
	request startRequest,
	rawInput *os.File,
) (*assignedLauncher, error) {
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create launcher event pipe: %w", err)
	}
	cleanupPipe := true
	defer func() {
		if cleanupPipe {
			_ = eventReader.Close()
			_ = eventWriter.Close()
		}
	}()
	if err := windows.SetHandleInformation(windows.Handle(eventWriter.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return nil, fmt.Errorf("make launcher event handle inheritable: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve supervisor executable: %w", err)
	}
	launcherArguments := []string{
		commandLauncher,
		"--event-handle", strconv.FormatUint(uint64(eventWriter.Fd()), 10),
	}
	inheritedHandles := []syscall.Handle{syscall.Handle(eventWriter.Fd())}
	if rawInput != nil {
		rawHandle := windows.Handle(rawInput.Fd())
		if err := windows.SetHandleInformation(
			rawHandle,
			windows.HANDLE_FLAG_INHERIT,
			windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			return nil, fmt.Errorf("make raw stdin handle inheritable: %w", err)
		}
		defer func() {
			_ = windows.SetHandleInformation(rawHandle, windows.HANDLE_FLAG_INHERIT, 0)
			_ = rawInput.Close()
		}()
		launcherArguments = append(
			launcherArguments,
			"--stdin-handle", strconv.FormatUint(uint64(rawInput.Fd()), 10),
		)
		inheritedHandles = append(inheritedHandles, syscall.Handle(rawInput.Fd()))
	}
	command := exec.Command(executable, launcherArguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:                 true,
		AdditionalInheritedHandles: inheritedHandles,
	}
	launcherInput, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create launcher input pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = launcherInput.Close()
		return nil, fmt.Errorf("start trusted launcher: %w", err)
	}
	_ = windows.SetHandleInformation(windows.Handle(eventWriter.Fd()), windows.HANDLE_FLAG_INHERIT, 0)
	_ = eventWriter.Close()

	var assignmentErr error
	var membershipHandle windows.Handle
	var duplicateErr error
	withHandleErr := command.Process.WithHandle(func(handle uintptr) {
		assignmentErr = windows.AssignProcessToJobObject(job.handle, windows.Handle(handle))
		if assignmentErr != nil {
			return
		}
		duplicateErr = windows.DuplicateHandle(
			windows.CurrentProcess(),
			windows.Handle(handle),
			windows.CurrentProcess(),
			&membershipHandle,
			0,
			false,
			windows.DUPLICATE_SAME_ACCESS,
		)
	})
	if withHandleErr != nil || assignmentErr != nil || duplicateErr != nil {
		_ = launcherInput.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		if membershipHandle != 0 {
			_ = windows.CloseHandle(membershipHandle)
		}
		if assignmentErr != nil {
			return nil, fmt.Errorf("assign trusted launcher to Job Object: %w", assignmentErr)
		}
		if duplicateErr != nil {
			return nil, fmt.Errorf("retain trusted launcher identity: %w", duplicateErr)
		}
		return nil, fmt.Errorf("access trusted launcher process handle: %w", withHandleErr)
	}
	if membershipHandle == 0 || membershipHandle == windows.InvalidHandle {
		_ = launcherInput.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, errors.New("retained trusted launcher handle is invalid")
	}
	membershipTransferred := false
	defer func() {
		if !membershipTransferred {
			_ = windows.CloseHandle(membershipHandle)
		}
	}()
	active, err := job.activeProcessCount()
	if err != nil || active != 1 {
		_ = launcherInput.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("fresh Job Object active process count is %d after launcher assignment, expected 1", active)
	}
	// No target-bearing byte crosses this pipe until assignment and accounting
	// both prove that the trusted launcher is contained.
	if err := writeCanonicalFrame(launcherInput, request); err != nil {
		_ = launcherInput.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("send request to assigned launcher: %w", err)
	}
	waitChannel := make(chan error, 1)
	go func() { waitChannel <- command.Wait() }()
	cleanupPipe = false
	membershipTransferred = true
	return &assignedLauncher{
		eventReader:      eventReader,
		input:            launcherInput,
		process:          command.Process,
		membershipHandle: membershipHandle,
		wait:             waitChannel,
	}, nil
}

func watchParentProcess(request startRequest) (<-chan controlResult, func(), error) {
	parentPID := os.Getppid()
	if parentPID < 1 || uint64(parentPID) > maxWindowsProcessID {
		return nil, nil, errors.New("parent process identity is invalid")
	}
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(parentPID),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("retain parent process authority: %w", err)
	}
	if actual, identityErr := windows.GetProcessId(handle); identityErr != nil || actual != uint32(parentPID) {
		_ = windows.CloseHandle(handle)
		if identityErr != nil {
			return nil, nil, fmt.Errorf("authenticate parent process authority: %w", identityErr)
		}
		return nil, nil, errors.New("parent process identity changed while opened")
	}
	results := make(chan controlResult, 1)
	go func() {
		outcome, waitErr := windows.WaitForSingleObject(handle, windows.INFINITE)
		if waitErr != nil || outcome != windows.WAIT_OBJECT_0 {
			results <- controlResult{err: errors.Join(
				waitErr,
				errors.New("parent process liveness wait lost authority"),
			)}
			return
		}
		// A signaled handle proves that the exact retained parent ended. Treating
		// that event as an authenticated parent request lets the sole Job owner
		// finish cleanup and publish tree-empty evidence after its client exits.
		results <- controlResult{request: terminateRequest{
			SchemaVersion: protocolSchemaVersion,
			Type:          requestTypeTerminate,
			OperationID:   request.OperationID,
			Nonce:         request.Nonce,
			Reason:        terminateReasonParentRequest,
		}}
	}()
	var closeOnce sync.Once
	return results, func() { closeOnce.Do(func() { _ = windows.CloseHandle(handle) }) }, nil
}

func watchTerminationControl(
	controlPath string,
	request startRequest,
) (<-chan controlResult, func()) {
	results := make(chan controlResult, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		poll := time.NewTicker(jobPollInterval)
		defer poll.Stop()
		for {
			control, present, err := readTerminationControlFile(controlPath, request)
			if err != nil {
				select {
				case results <- controlResult{err: err}:
				case <-stop:
				}
				return
			}
			if present {
				select {
				case results <- controlResult{request: control}:
				case <-stop:
				}
				return
			}
			select {
			case <-poll.C:
			case <-stop:
				return
			}
		}
	}()
	return results, func() { stopOnce.Do(func() { close(stop) }) }
}

func readTerminationControlFile(
	path string,
	request startRequest,
) (terminateRequest, bool, error) {
	pathBefore, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return terminateRequest{}, false, nil
	}
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("inspect termination control: %w", err)
	}
	if !pathBefore.Mode().IsRegular() || pathBefore.Mode()&os.ModeSymlink != 0 {
		return terminateRequest{}, false, errors.New("termination control must be a regular no-follow file")
	}
	file, err := os.Open(path)
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("open termination control: %w", err)
	}
	defer file.Close()
	openedBefore, err := file.Stat()
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("inspect opened termination control: %w", err)
	}
	if !os.SameFile(pathBefore, openedBefore) || pathBefore.Size() != openedBefore.Size() ||
		!pathBefore.ModTime().Equal(openedBefore.ModTime()) {
		return terminateRequest{}, false, errors.New("termination control changed while it was opened")
	}
	reader := bufio.NewReaderSize(file, controlReaderBufferBytes)
	control, err := readCanonicalFrame[terminateRequest](reader, "termination control")
	if err != nil {
		return terminateRequest{}, false, err
	}
	if trailing, trailingErr := reader.ReadByte(); !errors.Is(trailingErr, io.EOF) || trailing != 0 {
		return terminateRequest{}, false, errors.New("termination control contains trailing bytes")
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("reinspect opened termination control: %w", err)
	}
	pathAfter, err := os.Lstat(path)
	if err != nil {
		return terminateRequest{}, false, fmt.Errorf("reinspect termination control path: %w", err)
	}
	if !os.SameFile(openedBefore, openedAfter) || !os.SameFile(openedAfter, pathAfter) ||
		openedBefore.Size() != openedAfter.Size() || openedAfter.Size() != pathAfter.Size() ||
		!openedBefore.ModTime().Equal(openedAfter.ModTime()) ||
		!openedAfter.ModTime().Equal(pathAfter.ModTime()) {
		return terminateRequest{}, false, errors.New("termination control changed while it was read")
	}
	if err := validateTerminateRequest(control, request); err != nil {
		return terminateRequest{}, false, err
	}
	return control, true, nil
}

func mergeControlAuthorities(
	sources ...<-chan controlResult,
) (<-chan controlResult, func()) {
	results := make(chan controlResult, len(sources))
	stop := make(chan struct{})
	var stopOnce sync.Once
	for _, source := range sources {
		source := source
		go func() {
			select {
			case result := <-source:
				select {
				case results <- result:
				case <-stop:
				}
			case <-stop:
			}
		}()
	}
	return results, func() { stopOnce.Do(func() { close(stop) }) }
}

func requireExactRawEOF(reader io.Reader) error {
	buffer := make([]byte, 1)
	count, err := reader.Read(buffer)
	buffer[0] = 0
	if count != 0 || !errors.Is(err, io.EOF) {
		return errors.New("raw stdin pipe contains undeclared bytes")
	}
	return nil
}

func readExactRawStdin(handleValue uintptr, authority *rawStdin) ([]byte, error) {
	if authority == nil {
		if handleValue != 0 {
			return nil, errors.New("launcher received an undeclared raw stdin handle")
		}
		return nil, nil
	}
	if handleValue == 0 {
		return nil, errors.New("launcher did not receive its declared raw stdin handle")
	}
	handle := windows.Handle(handleValue)
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, fmt.Errorf("make launcher raw stdin handle private: %w", err)
	}
	reader := os.NewFile(handleValue, "windowsjob-raw-stdin")
	if reader == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("launcher raw stdin handle is invalid")
	}
	defer reader.Close()
	buffer := make([]byte, int(authority.ByteLength))
	success := false
	defer func() {
		if !success {
			for index := range buffer {
				buffer[index] = 0
			}
		}
	}()
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return nil, fmt.Errorf("read declared raw stdin bytes: %w", err)
	}
	extra := make([]byte, 1)
	count, err := reader.Read(extra)
	extra[0] = 0
	if count != 0 || !errors.Is(err, io.EOF) {
		return nil, errors.New("raw stdin pipe exceeds its declared byte length")
	}
	success = true
	return buffer, nil
}

func readLauncherEvent(reader io.Reader) (launcherEvent, error) {
	event, err := readCanonicalFrame[launcherEvent](reader, "launcher event")
	if err != nil {
		return launcherEvent{}, err
	}
	if event.SchemaVersion != protocolSchemaVersion {
		return launcherEvent{}, errors.New("launcher event schema is unsupported")
	}
	switch event.Type {
	case launcherEventRootStarted:
		if event.PID == 0 || event.ProcessHandle == 0 || event.SpawnFailure != nil {
			return launcherEvent{}, errors.New("root-started launcher event is inconsistent")
		}
	case launcherEventSpawnFailed:
		if event.PID != 0 || event.ProcessHandle != 0 || event.SpawnFailure == nil || *event.SpawnFailure == "" {
			return launcherEvent{}, errors.New("spawn-failed launcher event is inconsistent")
		}
		if boundedDiagnostic(errors.New(*event.SpawnFailure)) != *event.SpawnFailure {
			return launcherEvent{}, errors.New("spawn-failed launcher diagnostic is not canonical bounded text")
		}
	default:
		return launcherEvent{}, errors.New("launcher event type is unsupported")
	}
	return event, nil
}

func readControlRequests(reader io.Reader, start startRequest) <-chan controlResult {
	results := make(chan controlResult, 1)
	go func() {
		request, err := readCanonicalFrame[terminateRequest](reader, "control request")
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("parent control channel disconnected")
			}
			results <- controlResult{err: err}
			return
		}
		if err := validateTerminateRequest(request, start); err != nil {
			results <- controlResult{err: err}
			return
		}
		results <- controlResult{request: request}
	}()
	return results
}

func superviseLaunchedTree(
	job managedJob,
	request startRequest,
	statusPath string,
	event launcherEvent,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcher *assignedLauncher,
) error {
	if event.Type == launcherEventSpawnFailed {
		_ = launcher.input.Close()
		return superviseSpawnFailure(job, request, statusPath, event, deadline, controls, launcher.wait)
	}
	rootHandle, err := rootHandleFromEvent(job, event, launcher.process)
	if err != nil {
		return terminateAfterAuthorityFailure(job, request, err)
	}
	rootProbeHandle, err := duplicateLocalProcessHandle(rootHandle)
	if err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("retain root liveness probe: %w", err))
	}
	defer windows.CloseHandle(rootProbeHandle)
	if err := writeAll(launcher.input, []byte{launcherRootAcknowledgement}); err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("acknowledge root handle ownership: %w", err))
	}
	if err := launcher.input.Close(); err != nil {
		_ = windows.CloseHandle(rootHandle)
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("close launcher acknowledgement channel: %w", err))
	}
	rootExit := make(chan rootExitResult, 1)
	go func() {
		status, waitErr := waitRootAndClose(rootHandle, event.PID)
		rootExit <- rootExitResult{status: status, err: waitErr}
	}()
	handoff, err := awaitTrustedLauncherHandoff(job, request, deadline, controls, launcher)
	if err != nil {
		return err
	}
	if handoff.control != nil {
		replayedControl := make(chan controlResult, 1)
		replayedControl <- *handoff.control
		controls = replayedControl
	}
	if handoff.deadlineArrived {
		immediateDeadline := time.NewTimer(0)
		defer immediateDeadline.Stop()
		deadline = immediateDeadline
	}
	return superviseRootTree(
		job,
		managedRoot{handle: rootProbeHandle, pid: event.PID},
		request,
		statusPath,
		deadline,
		controls,
		rootExit,
	)
}

func awaitTrustedLauncherHandoff(
	job managedJob,
	request startRequest,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcher *assignedLauncher,
) (launcherHandoffResult, error) {
	// Prefer an already completed handoff over a simultaneously ready deadline
	// or control frame. Once Wait succeeds, Job accounting excludes launcher
	// infrastructure and those events can be judged against the target tree.
	select {
	case waitErr := <-launcher.wait:
		if err := completeTrustedLauncherHandoff(job, request, launcher, waitErr); err != nil {
			return launcherHandoffResult{}, err
		}
		return launcherHandoffResult{}, nil
	default:
	}
	select {
	case waitErr := <-launcher.wait:
		if err := completeTrustedLauncherHandoff(job, request, launcher, waitErr); err != nil {
			return launcherHandoffResult{}, err
		}
		return launcherHandoffResult{}, nil
	case control := <-controls:
		if control.err != nil {
			return launcherHandoffResult{}, terminateAfterAuthorityFailure(job, request, control.err)
		}
		if err := finishTrustedLauncherHandoff(job, request, launcher); err != nil {
			return launcherHandoffResult{}, err
		}
		return launcherHandoffResult{control: &control}, nil
	case <-deadline.C:
		if err := finishTrustedLauncherHandoff(job, request, launcher); err != nil {
			return launcherHandoffResult{}, err
		}
		return launcherHandoffResult{deadlineArrived: true}, nil
	}
}

func finishTrustedLauncherHandoff(
	job managedJob,
	request startRequest,
	launcher *assignedLauncher,
) error {
	waitLimit := time.NewTimer(time.Duration(request.TerminationGraceMS) * time.Millisecond)
	defer waitLimit.Stop()
	select {
	case waitErr := <-launcher.wait:
		return completeTrustedLauncherHandoff(job, request, launcher, waitErr)
	case <-waitLimit.C:
		return terminateAfterAuthorityFailure(job, request, errors.New("trusted launcher handoff did not complete within termination grace"))
	}
}

func completeTrustedLauncherHandoff(
	job managedJob,
	request startRequest,
	launcher *assignedLauncher,
	waitErr error,
) error {
	if waitErr != nil {
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("trusted launcher failed during root handoff: %w", waitErr))
	}
	launcherPID := launcher.process.Pid
	if launcherPID <= 0 || uint64(launcherPID) > maxWindowsProcessID {
		return terminateAfterAuthorityFailure(job, request, errors.New("trusted launcher PID is invalid"))
	}
	retainedPID, err := windows.GetProcessId(launcher.membershipHandle)
	if err != nil || retainedPID != uint32(launcherPID) {
		if err == nil {
			err = errors.New("retained trusted launcher identity changed")
		}
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("verify trusted launcher identity: %w", err))
	}
	if err := waitForProcessMembershipRelease(
		job,
		retainedPID,
		maxTerminationSnapshotProcesses,
		time.Duration(request.TerminationGraceMS)*time.Millisecond,
	); err != nil {
		return terminateAfterAuthorityFailure(job, request, fmt.Errorf("fence trusted launcher membership: %w", err))
	}
	return nil
}

func waitForProcessMembershipRelease(
	job managedJob,
	processID uint32,
	maximumProcesses int,
	maximumWait time.Duration,
) error {
	deadline := time.NewTimer(maximumWait)
	defer deadline.Stop()
	poll := time.NewTicker(jobPollInterval)
	defer poll.Stop()
	for {
		processIDs, err := job.activeProcessIDs(maximumProcesses)
		if err != nil {
			return err
		}
		found := false
		for _, activeProcessID := range processIDs {
			if activeProcessID == processID {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		select {
		case <-deadline.C:
			return errors.New("trusted launcher remained in the Job process list beyond termination grace")
		case <-poll.C:
		}
	}
}

func superviseRootTree(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request startRequest,
	statusPath string,
	deadline *time.Timer,
	controls <-chan controlResult,
	rootExit <-chan rootExitResult,
) error {
	return superviseRootTreeWithPollInterval(
		job,
		rootAuthority,
		request,
		statusPath,
		deadline,
		controls,
		rootExit,
		jobPollInterval,
	)
}

func superviseRootTreeWithPollInterval(
	job jobLifecycleAuthority,
	rootAuthority rootLifecycleAuthority,
	request startRequest,
	statusPath string,
	deadline *time.Timer,
	controls <-chan controlResult,
	rootExit <-chan rootExitResult,
	pollInterval time.Duration,
) error {
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	var root *rootStatus
	var terminationReason = terminationReasonNatural
	var timedOut bool
	var terminating bool
	var terminationDeadline <-chan time.Time
	var terminationLimit time.Time
	var fatalControl error
	var pendingIntervention *terminationIntervention
	defer func() {
		if pendingIntervention != nil {
			pendingIntervention.snapshot.close()
		}
	}()
	for {
		active, err := job.activeProcessCount()
		if err != nil {
			return err
		}
		if active == 0 {
			if fatalControl != nil {
				return fatalControl
			}
			if root == nil {
				select {
				case result := <-rootExit:
					if result.err != nil {
						return result.err
					}
					root = &result.status
				case <-time.After(time.Duration(request.TerminationGraceMS) * time.Millisecond):
					return errors.New("Job Object became empty before exact root status was available")
				}
			}
			if pendingIntervention != nil {
				terminationReason, timedOut, err = reconcileTerminationIntervention(
					job,
					*pendingIntervention,
					positiveDurationUntil(terminationLimit),
				)
				if err != nil {
					return err
				}
			}
			return publishStatusNew(statusPath, supervisorStatus{
				SchemaVersion:      protocolSchemaVersion,
				OperationID:        request.OperationID,
				Nonce:              request.Nonce,
				SupervisionOutcome: statusOutcomeTreeEmpty,
				TerminationReason:  terminationReason,
				TimedOut:           timedOut,
				ActiveProcessCount: 0,
				InputOutcome:       settledInputOutcome(request),
				Root:               root,
				SpawnFailure:       nil,
			})
		}
		select {
		case result := <-rootExit:
			if result.err != nil {
				return terminateAfterAuthorityFailure(job, request, result.err)
			}
			root = &result.status
			rootExit = nil
		case control := <-controls:
			controls = nil
			if control.err != nil {
				fatalControl = control.err
				if !terminating {
					if err := job.terminate(job.exitCodes().authority); err != nil {
						return err
					}
					terminating = true
					terminationLimit = time.Now().Add(time.Duration(request.TerminationGraceMS) * time.Millisecond)
					terminationDeadline = time.After(positiveDurationUntil(terminationLimit))
				}
				continue
			}
			if !terminating {
				intervention, err := terminateObservedNonemptyJob(
					job,
					rootAuthority,
					job.exitCodes().parent,
					terminateReasonParentRequest,
					false,
				)
				if err != nil {
					return err
				}
				if !intervention.applied {
					continue
				}
				pendingIntervention = &intervention
				terminating = true
				terminationLimit = time.Now().Add(time.Duration(request.TerminationGraceMS) * time.Millisecond)
				terminationDeadline = time.After(positiveDurationUntil(terminationLimit))
			}
		case <-deadline.C:
			if !terminating {
				intervention, err := terminateObservedNonemptyJob(
					job,
					rootAuthority,
					job.exitCodes().deadline,
					terminationReasonDeadline,
					true,
				)
				if err != nil {
					return err
				}
				if !intervention.applied {
					continue
				}
				pendingIntervention = &intervention
				terminating = true
				terminationLimit = time.Now().Add(time.Duration(request.TerminationGraceMS) * time.Millisecond)
				terminationDeadline = time.After(positiveDurationUntil(terminationLimit))
			}
		case <-terminationDeadline:
			return errors.New("Job Object did not become empty within termination grace")
		case <-poll.C:
		}
	}
}

func terminateObservedNonemptyJob(
	job jobLifecycleAuthority,
	root rootLifecycleAuthority,
	exitCode uint32,
	reason string,
	timedOut bool,
) (terminationIntervention, error) {
	// Aggregate Job counters can lag process signaling and cannot attribute an
	// exit to our request. Retained per-member handles turn termination into a
	// provisional intervention whose cause is authenticated only after Job-empty.
	snapshot, err := job.captureTerminationSnapshot(root, maxTerminationSnapshotProcesses)
	if err != nil {
		return terminationIntervention{}, err
	}
	if len(snapshot.members) == 0 {
		return terminationIntervention{}, nil
	}
	if err := job.terminate(exitCode); err != nil {
		snapshot.close()
		return terminationIntervention{}, err
	}
	return terminationIntervention{
		applied:  true,
		exitCode: exitCode,
		snapshot: snapshot,
		reason:   reason,
		timedOut: timedOut,
	}, nil
}

func reconcileTerminationIntervention(
	job jobLifecycleAuthority,
	intervention terminationIntervention,
	maximumEvidenceWait time.Duration,
) (string, bool, error) {
	accounting, err := job.processAccounting()
	if err != nil {
		return "", false, err
	}
	if accounting.total != intervention.snapshot.totalProcessesBefore {
		return "", false, errors.New("termination causality was invalidated by concurrent target process creation")
	}
	codes := job.exitCodes()
	interventionObserved := false
	evidenceDeadline := time.Now().Add(maximumEvidenceWait)
	for _, member := range intervention.snapshot.members {
		exitCode, err := member.exactExitCode(positiveDurationUntil(evidenceDeadline))
		if err != nil {
			return "", false, err
		}
		switch exitCode {
		case intervention.exitCode:
			interventionObserved = true
		case codes.deadline, codes.parent, codes.authority:
			return "", false, fmt.Errorf("process %d exited with an unexpected private termination code", member.processID())
		}
	}
	if interventionObserved {
		return intervention.reason, intervention.timedOut, nil
	}
	// TerminateJobObject accepted the request, but every process retained from
	// the pre-call snapshot exited with its own code. With an unchanged process
	// generation, natural completion is the only causal account of the tree.
	return terminationReasonNatural, false, nil
}

func superviseSpawnFailure(
	job managedJob,
	request startRequest,
	statusPath string,
	event launcherEvent,
	deadline *time.Timer,
	controls <-chan controlResult,
	launcherWait <-chan error,
) error {
	poll := time.NewTicker(jobPollInterval)
	defer poll.Stop()
	launcherReaped := false
	for {
		active, err := job.activeProcessCount()
		if err != nil {
			return err
		}
		if active == 0 && launcherReaped {
			return publishStatusNew(statusPath, supervisorStatus{
				SchemaVersion:      protocolSchemaVersion,
				OperationID:        request.OperationID,
				Nonce:              request.Nonce,
				SupervisionOutcome: statusOutcomeSpawnFailed,
				TerminationReason:  terminationReasonTargetSpawnFailed,
				TimedOut:           false,
				ActiveProcessCount: 0,
				InputOutcome:       inputOutcomeNotStarted,
				Root:               nil,
				SpawnFailure:       event.SpawnFailure,
			})
		}
		select {
		case waitErr := <-launcherWait:
			if waitErr != nil {
				return terminateAfterAuthorityFailure(job, request, fmt.Errorf("trusted launcher failed after spawn failure: %w", waitErr))
			}
			launcherReaped = true
			launcherWait = nil
		case control := <-controls:
			if control.err != nil {
				return terminateAfterAuthorityFailure(job, request, control.err)
			}
			return terminateAfterAuthorityFailure(job, request, errors.New("parent termination raced target spawn failure"))
		case <-deadline.C:
			return terminateAfterAuthorityFailure(job, request, errors.New("trusted launcher did not drain after target spawn failure"))
		case <-poll.C:
		}
	}
}

func terminatePendingLaunch(
	job managedJob,
	request startRequest,
	statusPath, reason string,
	timedOut bool,
	events <-chan launcherEventResult,
	launcherWait <-chan error,
) error {
	exitCode := job.exitCodes().parent
	if timedOut {
		exitCode = job.exitCodes().deadline
	}
	if err := job.terminate(exitCode); err != nil {
		return err
	}
	authorityDeadline := time.Now().Add(time.Duration(request.TerminationGraceMS) * time.Millisecond)
	var event launcherEvent
	select {
	case eventResult := <-events:
		if eventResult.err != nil {
			_ = waitForJobEmpty(job, time.Until(authorityDeadline))
			return eventResult.err
		}
		event = eventResult.event
	case <-time.After(positiveDurationUntil(authorityDeadline)):
		return errors.New("launcher event was unavailable within termination grace")
	}
	select {
	case <-launcherWait:
	case <-time.After(positiveDurationUntil(authorityDeadline)):
		return errors.New("trusted launcher was not reaped within termination grace")
	}
	if err := waitForJobEmpty(job, positiveDurationUntil(authorityDeadline)); err != nil {
		return err
	}
	if event.Type == launcherEventSpawnFailed {
		return publishStatusNew(statusPath, supervisorStatus{
			SchemaVersion:      protocolSchemaVersion,
			OperationID:        request.OperationID,
			Nonce:              request.Nonce,
			SupervisionOutcome: statusOutcomeSpawnFailed,
			TerminationReason:  terminationReasonTargetSpawnFailed,
			TimedOut:           false,
			ActiveProcessCount: 0,
			InputOutcome:       inputOutcomeNotStarted,
			Root:               nil,
			SpawnFailure:       event.SpawnFailure,
		})
	}
	// A root-started event requires the launcher ACK transaction. If termination
	// won the race before that transaction, exact root status cannot be recovered.
	return errors.New("root handle transfer did not complete before termination")
}

func settledInputOutcome(request startRequest) string {
	if request.Stdin == nil {
		return inputOutcomeNotRequested
	}
	return inputOutcomeDelivered
}

func positiveDurationUntil(deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if remaining > 0 {
		return remaining
	}
	return time.Nanosecond
}

func terminateAfterAuthorityFailure(job jobLifecycleAuthority, request startRequest, cause error) error {
	_ = job.terminate(job.exitCodes().authority)
	_ = waitForJobEmpty(job, time.Duration(request.TerminationGraceMS)*time.Millisecond)
	return cause
}

func waitForJobEmpty(job jobLifecycleAuthority, maximum time.Duration) error {
	deadline := time.Now().Add(maximum)
	for {
		active, err := job.activeProcessCount()
		if err != nil {
			return err
		}
		if active == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("Job Object did not become empty within termination grace")
		}
		time.Sleep(jobPollInterval)
	}
}

func rootHandleFromEvent(job managedJob, event launcherEvent, launcher *os.Process) (windows.Handle, error) {
	if uint64(uintptr(event.ProcessHandle)) != event.ProcessHandle {
		return 0, errors.New("launcher-local root process handle does not fit this architecture")
	}
	sourceHandle := windows.Handle(uintptr(event.ProcessHandle))
	if sourceHandle == 0 || sourceHandle == windows.InvalidHandle {
		return 0, errors.New("launcher-local root process handle is invalid")
	}
	var handle windows.Handle
	var duplicateErr error
	withHandleErr := launcher.WithHandle(func(launcherHandle uintptr) {
		duplicateErr = windows.DuplicateHandle(
			windows.Handle(launcherHandle),
			sourceHandle,
			windows.CurrentProcess(),
			&handle,
			0,
			false,
			windows.DUPLICATE_SAME_ACCESS,
		)
	})
	if withHandleErr != nil {
		return 0, fmt.Errorf("access launcher for root handle transfer: %w", withHandleErr)
	}
	if duplicateErr != nil {
		return 0, fmt.Errorf("transfer root handle from launcher: %w", duplicateErr)
	}
	if handle == 0 || handle == windows.InvalidHandle {
		return 0, errors.New("transferred root process handle is invalid")
	}
	pid, err := windows.GetProcessId(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("read duplicated root PID: %w", err)
	}
	if pid != event.PID {
		_ = windows.CloseHandle(handle)
		return 0, errors.New("duplicated root handle PID does not match launcher event")
	}
	member, err := isProcessInJob(handle, job.handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	if !member {
		_ = windows.CloseHandle(handle)
		return 0, errors.New("root process did not inherit the managed Job Object")
	}
	return handle, nil
}

func duplicateLocalProcessHandle(source windows.Handle) (windows.Handle, error) {
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		source,
		windows.CurrentProcess(),
		&duplicate,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return 0, err
	}
	if duplicate == 0 || duplicate == windows.InvalidHandle {
		return 0, errors.New("duplicated process handle is invalid")
	}
	return duplicate, nil
}

func (root managedRoot) processID() uint32 {
	return root.pid
}

func (root managedRoot) retainExitAuthority() (processExitAuthority, error) {
	handle, err := duplicateLocalProcessHandle(root.handle)
	if err != nil {
		return nil, err
	}
	authority := &managedProcessExitAuthority{handle: handle, pid: root.pid}
	processID, err := windows.GetProcessId(handle)
	if err != nil {
		authority.close()
		return nil, err
	}
	if processID != root.pid {
		authority.close()
		return nil, errors.New("retained root process identity changed")
	}
	return authority, nil
}

func waitRootAndClose(handle windows.Handle, pid uint32) (rootStatus, error) {
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if err != nil {
		return rootStatus{}, fmt.Errorf("wait for root process: %w", err)
	}
	if result != windows.WAIT_OBJECT_0 {
		return rootStatus{}, fmt.Errorf("wait for root process returned %#x", result)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return rootStatus{}, fmt.Errorf("read exact root exit code: %w", err)
	}
	return rootStatus{PID: pid, ExitCode: exitCode}, nil
}

func verifyJobHandleNonInheritable(handle windows.Handle) error {
	var flags uint32
	result, _, callErr := getHandleInformationProcedure.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&flags)),
	)
	runtime.KeepAlive(&flags)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = syscall.EINVAL
		}
		return fmt.Errorf("read back Job Object handle flags: %w", callErr)
	}
	if flags != 0 {
		return fmt.Errorf("fresh Job Object handle flags read back as %#x, expected 0", flags)
	}
	return nil
}

func isProcessInJob(process, job windows.Handle) (bool, error) {
	var member int32
	result, _, callErr := isProcessInJobProcedure.Call(
		uintptr(process),
		uintptr(job),
		uintptr(unsafe.Pointer(&member)),
	)
	runtime.KeepAlive(&member)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = syscall.EINVAL
		}
		return false, fmt.Errorf("verify process Job Object membership: %w", callErr)
	}
	return member != 0, nil
}

func validateWindowsEnvironment(environment []environmentEntry) error {
	for right := 1; right < len(environment); right++ {
		for left := 0; left < right; left++ {
			equal, err := windowsEnvironmentNamesEqual(environment[left].Name, environment[right].Name)
			if err != nil {
				return err
			}
			if equal {
				return fmt.Errorf("environment contains Windows-case-insensitive duplicate %q", environment[right].Name)
			}
		}
	}
	return nil
}

func windowsEnvironmentNamesEqual(left, right string) (bool, error) {
	leftUTF16, err := windows.UTF16FromString(left)
	if err != nil {
		return false, fmt.Errorf("encode environment name %q: %w", left, err)
	}
	rightUTF16, err := windows.UTF16FromString(right)
	if err != nil {
		return false, fmt.Errorf("encode environment name %q: %w", right, err)
	}
	result, _, callErr := compareStringOrdinalProcedure.Call(
		uintptr(unsafe.Pointer(&leftUTF16[0])),
		uintptr(len(leftUTF16)-1),
		uintptr(unsafe.Pointer(&rightUTF16[0])),
		uintptr(len(rightUTF16)-1),
		1,
	)
	runtime.KeepAlive(leftUTF16)
	runtime.KeepAlive(rightUTF16)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = syscall.EINVAL
		}
		return false, fmt.Errorf("compare Windows environment names: %w", callErr)
	}
	return result == compareStringEqual, nil
}
