//go:build windows

package windowsjob

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const (
	jobPollInterval             = 20 * time.Millisecond
	initialJobProcessIDCapacity = 8
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

type jobProcessAccounting struct {
	total  uint32
	active uint32
}

type jobProcessIDQuery struct {
	storage     []uintptr
	headerWords int
	capacity    int
	assigned    uint64
	listed      uint64
	succeeded   bool
	callErr     error
}

// jobLifecycleAuthority is consumed only after launcher identity is fenced out
// of the Job. Termination remains provisional until exact retained member exits
// and the process-generation counter jointly authenticate its cause.

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
		return fmt.Errorf("job object limits read back as %#x, expected exact non-breakaway kill-on-close %#x",
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
		return nil, errors.New("job process snapshot limit must be positive")
	}
	capacity := min(initialJobProcessIDCapacity, maximumProcesses)
	for {
		query := job.queryActiveProcessIDs(capacity)
		if err := query.validateEvidenceLimit(maximumProcesses); err != nil {
			return nil, err
		}
		if query.succeeded {
			return query.validatedProcessIDs()
		}
		nextCapacity, err := query.retryCapacity(maximumProcesses)
		if err != nil {
			return nil, err
		}
		capacity = nextCapacity
	}
}

func (job managedJob) queryActiveProcessIDs(capacity int) jobProcessIDQuery {
	pointerBytes := unsafe.Sizeof(uintptr(0))
	headerWords := int((unsafe.Sizeof(jobObjectBasicProcessIDListHeader{}) + pointerBytes - 1) / pointerBytes)
	storage := make([]uintptr, headerWords+capacity)
	header := (*jobObjectBasicProcessIDListHeader)(unsafe.Pointer(&storage[0]))
	var returnedLength uint32
	result, _, callErr := queryJobInformationProcedure.Call(
		uintptr(job.handle),
		uintptr(windows.JobObjectBasicProcessIdList),
		uintptr(unsafe.Pointer(&storage[0])),
		uintptr(len(storage))*pointerBytes,
		uintptr(unsafe.Pointer(&returnedLength)),
	)
	query := jobProcessIDQuery{
		storage: storage, headerWords: headerWords, capacity: capacity,
		assigned:  uint64(header.numberOfAssignedProcesses),
		listed:    uint64(header.numberOfProcessIDsInList),
		succeeded: result != 0, callErr: callErr,
	}
	runtime.KeepAlive(storage)
	return query
}

func (query jobProcessIDQuery) validateEvidenceLimit(maximumProcesses int) error {
	if query.assigned > uint64(maximumProcesses) || query.listed > uint64(maximumProcesses) {
		return jobProcessEvidenceLimitError(maximumProcesses)
	}
	return nil
}

func (query jobProcessIDQuery) retryCapacity(maximumProcesses int) (int, error) {
	if !errors.Is(query.callErr, windows.ERROR_MORE_DATA) {
		callErr := query.callErr
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = syscall.EINVAL
		}
		return 0, fmt.Errorf("query Job Object process IDs: %w", callErr)
	}
	nextCapacity := int(query.assigned)
	if nextCapacity > query.capacity {
		return nextCapacity, nil
	}
	if query.capacity > maximumProcesses/2 {
		return 0, jobProcessEvidenceLimitError(maximumProcesses)
	}
	return query.capacity * 2, nil
}

func (query jobProcessIDQuery) validatedProcessIDs() ([]uint32, error) {
	// The kernel writes the header and pointer-width IDs into one allocation;
	// retaining it through validation keeps the unsafe view authoritative.
	defer runtime.KeepAlive(query.storage)
	if query.listed > uint64(query.capacity) || query.listed != query.assigned {
		return nil, errors.New("job object returned an incomplete process ID snapshot")
	}
	ids := make([]uint32, int(query.listed))
	for index := range ids {
		word := query.storage[query.headerWords+index]
		if uint64(word) == 0 || uint64(word) > maxWindowsProcessID {
			return nil, errors.New("job object returned an invalid process ID")
		}
		ids[index] = uint32(word)
	}
	return ids, nil
}

func jobProcessEvidenceLimitError(maximumProcesses int) error {
	return fmt.Errorf("job contains more than the termination evidence limit of %d processes", maximumProcesses)
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
