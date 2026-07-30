//go:build windows

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"time"
)

const (
	maxTerminationSnapshotProcesses             = 4_096
	terminationSnapshotExitChurnAttempts        = 3
	privateExitCodePayloadMask           uint32 = (1 << 29) - 1
	deadlineExitCodeDomain                      = "windshare/windowsjob/deadline-exit/v1"
	parentExitCodeDomain                        = "windshare/windowsjob/parent-exit/v1"
	authorityExitCodeDomain                     = "windshare/windowsjob/authority-exit/v1"
)

type terminationExitCodes struct {
	deadline  uint32
	parent    uint32
	authority uint32
}

type jobLifecycleAuthority interface {
	activeProcessCount() (uint32, error)
	captureTerminationSnapshot(root rootLifecycleAuthority, maximumProcesses int) (targetMemberSnapshot, error)
	processAccounting() (jobProcessAccounting, error)
	exitCodes() terminationExitCodes
	terminate(uint32) error
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

func (job managedJob) captureTerminationSnapshot(
	root rootLifecycleAuthority,
	maximumProcesses int,
) (targetMemberSnapshot, error) {
	accounting, err := job.processAccounting()
	if err != nil {
		return targetMemberSnapshot{}, err
	}
	for range terminationSnapshotExitChurnAttempts {
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
		return targetMemberSnapshot{}, false, errors.New("the Job Object process snapshot exceeds its total-process generation")
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
			resultErr = errors.New("the Job Object process snapshot contains a duplicate process ID")
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

func (snapshot *targetMemberSnapshot) close() {
	for _, member := range snapshot.members {
		member.close()
	}
	snapshot.members = nil
}

func (job managedJob) terminate(exitCode uint32) error {
	if err := windows.TerminateJobObject(job.handle, exitCode); err != nil {
		return fmt.Errorf("terminate Job Object: %w", err)
	}
	return nil
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
			return errors.New("the Job Object did not become empty within termination grace")
		}
		time.Sleep(jobPollInterval)
	}
}
