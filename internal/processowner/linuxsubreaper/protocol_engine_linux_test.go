//go:build linux

package linuxsubreaper

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/unix"
)

func TestRunMainRejectsNonCommands(t *testing.T) {
	for _, arguments := range [][]string{nil, {commandSupervise, commandExecChild}, {"unknown"}} {
		if err := runMain(arguments); err == nil {
			t.Fatalf("runMain(%q) accepted invalid command", arguments)
		}
	}
}

func TestExecutableHoldUsesRevisionIdentityWithoutReadingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	authority, err := holdExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.assertLive(); err != nil {
		authority.close()
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		authority.close()
		t.Fatal(err)
	}
	if err := authority.assertLive(); err == nil || !strings.Contains(err.Error(), "changed while held") {
		authority.close()
		t.Fatalf("revision replacement error = %v", err)
	}
	authority.close()

	if _, err := holdExecutable(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing executable was accepted")
	}
	nonExecutable := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(nonExecutable, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := holdExecutable(nonExecutable); err == nil {
		t.Fatal("non-executable file was accepted")
	}
	symlink := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := holdExecutable(symlink); err == nil {
		t.Fatal("symlink executable was accepted")
	}
}

func TestHeldDescriptorValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	authority, err := holdExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	file := authority.file
	if err := authenticateHeldExecutable(file); err != nil {
		authority.close()
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_PATH == 0 {
		authority.close()
		t.Fatalf("held executable flags = %#x, %v", flags, err)
	}
	authority.close()
	if err := authenticateHeldExecutable(file); err == nil {
		t.Fatal("closed descriptor was accepted")
	}
}

func TestExecutableHoldAcceptsExecuteOnlyAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execute-only")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o111); err != nil {
		t.Fatal(err)
	}
	authority, err := holdExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.close()
	if err := authority.assertLive(); err != nil {
		t.Fatal(err)
	}
}

func TestExecResultHandshakeDistinguishesSuccessFailureAndLostEvidence(t *testing.T) {
	if result := readExecResult(bytes.NewReader(nil)); result.err == nil || result.started || result.failure != nil {
		t.Fatalf("empty exec result = %#v", result)
	}
	if result := readExecResult(bytes.NewReader([]byte{execAttemptMarker})); !result.started || result.failure != nil || result.err != nil {
		t.Fatalf("successful exec result = %#v", result)
	}

	var failure bytes.Buffer
	if err := writeExecFailure(&failure, "TARGET_EXEC_FAILED", errors.New("permission denied")); err != nil {
		t.Fatal(err)
	}
	result := readExecResult(bytes.NewReader(failure.Bytes()))
	if result.started || result.err != nil || result.failure == nil ||
		result.failure.FailureCode != "TARGET_EXEC_FAILED" || result.failure.FailureMessage != "permission denied" {
		t.Fatalf("failed exec result = %#v", result)
	}

	malformed := make([]byte, 4)
	binary.BigEndian.PutUint32(malformed, ownerprotocol.MaximumDocumentBytes+1)
	if result := readExecResult(bytes.NewReader(malformed)); result.err == nil || result.started || result.failure != nil {
		t.Fatalf("malformed exec result = %#v", result)
	}
	trailing := append(bytes.Clone(failure.Bytes()), 0)
	if result := readExecResult(bytes.NewReader(trailing)); result.err == nil {
		t.Fatalf("trailing exec result = %#v", result)
	}
}

func TestExecResultApplicationPreservesControlledTermination(t *testing.T) {
	state := supervisionState{terminationReason: ownerprotocol.TerminationStop, launchPhase: launchGateReleased}
	state = applyExecResult(state, execResult{failure: &execFailure{
		FailureCode: "TARGET_EXEC_FAILED", FailureMessage: "permission denied",
	}})
	if state.terminationReason != ownerprotocol.TerminationStop || state.target.Outcome != ownerprotocol.TargetSpawnFailed ||
		!state.execResultObserved || state.launched() {
		t.Fatalf("controlled failed exec state = %#v", state)
	}

	lost := applyExecResult(supervisionState{
		terminationReason: ownerprotocol.TerminationNatural, launchPhase: launchGateReleased,
	}, execResult{
		err: errors.New("result pipe corrupted"),
	})
	if lost.terminationReason != ownerprotocol.TerminationOwnerFailure ||
		lost.target.Outcome != ownerprotocol.TargetStartEvidenceLost || lost.authorityFailure == nil {
		t.Fatalf("lost exec evidence state = %#v", lost)
	}
}

func TestLaunchPhaseLinearizesControlBeforeAndAfterGateRelease(t *testing.T) {
	controlBefore := make(chan string, 1)
	controlBefore <- ownerprotocol.TerminationStop
	before := awaitExecGate(
		ownerprotocol.Request{},
		nil,
		nil,
		nil,
		ownerprotocol.StartEvidence{},
		nil,
		make(chan error),
		make(chan execResult),
		make(chan terminalResult),
		controlBefore,
		make(chan time.Time),
	)
	if before.launchPhase != launchPrevented || before.target.Outcome != ownerprotocol.TargetNotStarted || before.launched() {
		t.Fatalf("pre-release stop state = %#v", before)
	}

	controlAfter := make(chan string, 1)
	controlAfter <- ownerprotocol.TerminationStop
	after := awaitExecConfirmation(
		supervisionState{terminationReason: ownerprotocol.TerminationNatural, launchPhase: launchGateReleased},
		make(chan execResult),
		make(chan terminalResult),
		controlAfter,
		make(chan time.Time),
	)
	if after.launchPhase != launchEvidenceLost || after.target.Outcome != ownerprotocol.TargetStartEvidenceLost ||
		after.terminationReason != ownerprotocol.TerminationStop || after.launched() {
		t.Fatalf("post-release stop state = %#v", after)
	}
}

func TestExecGateReadinessOutcomesRemainFailClosed(t *testing.T) {
	readyFailure := make(chan error, 1)
	readyFailure <- errors.New("gate did not authenticate")
	ownerFailure := make(chan string, 1)
	ownerFailure <- ownerprotocol.TerminationOwnerFailure
	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	terminal := make(chan terminalResult, 1)
	terminal <- terminalResult{evidence: ownerprotocol.TargetEvidence{Outcome: ownerprotocol.TargetExited}}

	testCases := []struct {
		name             string
		authorityFailure error
		ready            <-chan error
		wait             <-chan terminalResult
		control          <-chan string
		deadline         <-chan time.Time
		wantReason       string
		wantRootTerminal bool
	}{
		{name: "authority", authorityFailure: errors.New("subreaper unavailable"), wantReason: ownerprotocol.TerminationOwnerFailure},
		{name: "readiness", ready: readyFailure, wantReason: ownerprotocol.TerminationNatural},
		{name: "control authority", control: ownerFailure, wantReason: ownerprotocol.TerminationOwnerFailure},
		{name: "deadline", deadline: deadline, wantReason: ownerprotocol.TerminationDeadline},
		{name: "gate terminal", wait: terminal, wantReason: ownerprotocol.TerminationNatural, wantRootTerminal: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			state := awaitExecGate(
				ownerprotocol.Request{},
				testCase.authorityFailure,
				nil,
				nil,
				ownerprotocol.StartEvidence{},
				nil,
				testCase.ready,
				make(chan execResult),
				testCase.wait,
				testCase.control,
				testCase.deadline,
			)
			if state.launchPhase != launchPrevented || state.target.Outcome != ownerprotocol.TargetNotStarted ||
				state.terminationReason != testCase.wantReason || (state.rootTerminal != nil) != testCase.wantRootTerminal {
				t.Fatalf("gate readiness state = %#v", state)
			}
		})
	}
}

func TestTerminalExecResultResolutionCoversEveryAuthorityBoundary(t *testing.T) {
	started := make(chan execResult, 1)
	started <- execResult{started: true}
	state := awaitTerminalExecResult(
		supervisionState{terminationReason: ownerprotocol.TerminationNatural, launchPhase: launchGateReleased},
		started,
		nil,
		nil,
	)
	if !state.launched() || state.target.Outcome != "" {
		t.Fatalf("terminal success state = %#v", state)
	}

	control := make(chan string, 1)
	control <- ownerprotocol.TerminationStop
	state = awaitTerminalExecResult(
		supervisionState{terminationReason: ownerprotocol.TerminationNatural, launchPhase: launchGateReleased},
		make(chan execResult),
		control,
		nil,
	)
	if state.launchPhase != launchEvidenceLost || state.terminationReason != ownerprotocol.TerminationStop {
		t.Fatalf("terminal control state = %#v", state)
	}

	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	state = awaitTerminalExecResult(
		supervisionState{terminationReason: ownerprotocol.TerminationNatural, launchPhase: launchGateReleased},
		make(chan execResult),
		nil,
		deadline,
	)
	if state.launchPhase != launchEvidenceLost || state.terminationReason != ownerprotocol.TerminationDeadline {
		t.Fatalf("terminal deadline state = %#v", state)
	}

	startedAt := time.Now()
	state = awaitTerminalExecResult(
		supervisionState{terminationReason: ownerprotocol.TerminationNatural, launchPhase: launchGateReleased},
		make(chan execResult),
		nil,
		nil,
	)
	if elapsed := time.Since(startedAt); elapsed < execResultTerminalDrainLimit || elapsed > 2*time.Second {
		t.Fatalf("terminal evidence drain lease = %s", elapsed)
	}
	if state.launchPhase != launchEvidenceLost || state.terminationReason != ownerprotocol.TerminationOwnerFailure {
		t.Fatalf("terminal timeout state = %#v", state)
	}
}

func TestKernelTerminalRootWinsBeforeDelayedWaitPublication(t *testing.T) {
	testCases := []struct {
		name     string
		control  string
		deadline bool
	}{
		{name: "stop", control: ownerprotocol.TerminationStop},
		{name: "parent lost", control: ownerprotocol.TerminationParentLost},
		{name: "control authority failure", control: ownerprotocol.TerminationOwnerFailure},
		{name: "deadline", deadline: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			command := exec.Command("/bin/sh", "-c", "read _")
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			authority := newInventoryAuthority(os.Getpid())
			t.Cleanup(authority.close)
			if _, err := authenticateRootProcess(authority, command.Process.Pid); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatal(err)
			}
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			waitErr := command.Wait()
			terminal := terminalResult{
				evidence: terminalEvidence(command.ProcessState, waitErr),
				err:      waitErr,
			}

			observed := &observedOwnedTargetAuthority{
				inventory: authority,
				attempts:  make(chan terminationSignalWitness, 1),
			}
			wait := make(chan terminalResult, 1)
			publication := make(chan struct{})
			go func() {
				<-publication
				wait <- terminal
			}()
			control := make(chan string, 1)
			deadline := make(chan time.Time, 1)
			if testCase.deadline {
				deadline <- time.Now()
			} else {
				control <- testCase.control
			}
			finished := make(chan supervisionState, 1)
			go func() {
				finished <- monitorOwnedTarget(
					supervisionState{
						terminationReason: ownerprotocol.TerminationNatural,
						launchPhase:       launchConfirmed,
					},
					observed,
					wait,
					control,
					deadline,
					nil,
				)
			}()

			witness := <-observed.attempts
			if witness.applied() || witness.terminal == 0 {
				t.Fatalf("kernel-terminal signal witness = %#v", witness)
			}
			select {
			case state := <-finished:
				t.Fatalf("monitor settled before exact Wait publication: %#v", state)
			default:
			}
			close(publication)
			state := <-finished
			if state.terminationReason != ownerprotocol.TerminationNatural ||
				state.rootTerminal == nil || state.authorityFailure != nil {
				t.Fatalf("delayed natural terminal state = %#v", state)
			}
		})
	}
}

func TestUnavailablePidfdNeverFallsBackToNumericRootSignal(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "read _")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	authority := newInventoryAuthority(os.Getpid())
	defer authority.close()
	root, err := authenticateRootProcess(authority, command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	tracked := authority.tracked[identityKey(root)]
	if tracked == nil {
		t.Fatal("authenticated root lacks retained pidfd authority")
	}
	if err := unix.Close(tracked.pidfd); err != nil {
		t.Fatal(err)
	}

	witness, err := authority.signalTrackedWithWitness(unix.SIGTERM)
	if err == nil || witness.applied() {
		t.Fatalf("unavailable pidfd signal = (%#v, %v)", witness, err)
	}
	if err := unix.Kill(command.Process.Pid, 0); err != nil {
		t.Fatalf("root was numerically signaled after exact authority failed: %v", err)
	}
}

type observedOwnedTargetAuthority struct {
	inventory *inventoryAuthority
	attempts  chan terminationSignalWitness
}

func (authority *observedOwnedTargetAuthority) refreshOwnedTree() error {
	return authority.inventory.refreshOwnedTree()
}

func (authority *observedOwnedTargetAuthority) requestTermination(
	signal unix.Signal,
) (terminationSignalWitness, error) {
	witness, err := authority.inventory.requestTermination(signal)
	authority.attempts <- witness
	return witness, err
}

func (authority *observedOwnedTargetAuthority) naturalTreeComplete() (bool, error) {
	return authority.inventory.naturalTreeComplete()
}

func TestReleaseExecGateLinearizesAuthorityControlAndPublication(t *testing.T) {
	executablePath := filepath.Join(t.TempDir(), "target")
	copyExecutable(t, "/proc/self/exe", executablePath)
	if err := os.Chmod(executablePath, 0o700); err != nil {
		t.Fatal(err)
	}
	newAuthority := func(t *testing.T) *executableAuthority {
		t.Helper()
		authority, err := holdExecutable(executablePath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(authority.close)
		return authority
	}

	t.Run("stale authority", func(t *testing.T) {
		authority := newAuthority(t)
		if err := os.Rename(executablePath, executablePath+"-replaced"); err != nil {
			t.Fatal(err)
		}
		defer os.Rename(executablePath+"-replaced", executablePath)
		state := releaseExecGate(
			supervisionState{terminationReason: ownerprotocol.TerminationNatural},
			authority, nil, nil, nil, nil, nil,
		)
		if state.launchPhase != launchPrevented || state.terminationReason != ownerprotocol.TerminationOwnerFailure {
			t.Fatalf("stale-authority state = %#v", state)
		}
	})

	t.Run("control before release", func(t *testing.T) {
		control := make(chan string, 1)
		control <- ownerprotocol.TerminationStop
		state := releaseExecGate(
			supervisionState{terminationReason: ownerprotocol.TerminationNatural},
			newAuthority(t), nil, nil, nil, control, nil,
		)
		if state.launchPhase != launchPrevented || state.terminationReason != ownerprotocol.TerminationStop {
			t.Fatalf("pre-release control state = %#v", state)
		}
	})

	t.Run("deadline before release", func(t *testing.T) {
		deadline := make(chan time.Time, 1)
		deadline <- time.Now()
		state := releaseExecGate(
			supervisionState{terminationReason: ownerprotocol.TerminationNatural},
			newAuthority(t), nil, nil, nil, nil, deadline,
		)
		if state.launchPhase != launchPrevented || state.terminationReason != ownerprotocol.TerminationDeadline {
			t.Fatalf("pre-release deadline state = %#v", state)
		}
	})

	t.Run("publication failure", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
		_ = writer.Close()
		state := releaseExecGate(
			supervisionState{terminationReason: ownerprotocol.TerminationNatural},
			newAuthority(t), writer, nil, nil, nil, nil,
		)
		if state.launchPhase != launchPrevented || state.terminationReason != ownerprotocol.TerminationOwnerFailure {
			t.Fatalf("release failure state = %#v", state)
		}
	})

	t.Run("confirmed release", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		results := make(chan execResult, 1)
		results <- execResult{started: true}
		state := releaseExecGate(
			supervisionState{terminationReason: ownerprotocol.TerminationNatural},
			newAuthority(t), writer, results, nil, nil, nil,
		)
		published, readErr := io.ReadAll(reader)
		if readErr != nil || !bytes.Equal(published, []byte{1}) {
			t.Fatalf("release publication = %v, %v", published, readErr)
		}
		if !state.launched() || state.terminationReason != ownerprotocol.TerminationNatural {
			t.Fatalf("confirmed release state = %#v", state)
		}
	})
}

func TestExecResultResolutionIsBoundedAndLateSuccessCannotUpgradeLostEvidence(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	results := make(chan execResult, 1)
	readerDone := make(chan struct{})
	go func() {
		results <- readExecResult(reader)
		close(readerDone)
	}()
	started := time.Now()
	state := resolveExecResult(
		supervisionState{terminationReason: ownerprotocol.TerminationStop, launchPhase: launchGateReleased},
		results,
		reader,
		time.Now().Add(25*time.Millisecond),
	)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded exec-result resolution took %s", elapsed)
	}
	if state.launchPhase != launchEvidenceLost || state.target.Outcome != ownerprotocol.TargetStartEvidenceLost {
		t.Fatalf("timed-out launch state = %#v", state)
	}
	select {
	case <-readerDone:
	case <-time.After(time.Second):
		t.Fatal("closing the result source did not retire its reader goroutine")
	}
	if _, err := reader.Stat(); err == nil {
		t.Fatal("timed-out result source remained open")
	}
	state.execResultObserved = false
	state = resolveExecResult(state, results, reader, time.Now().Add(time.Second))
	lateSuccess := make(chan execResult, 1)
	lateSuccess <- execResult{started: true}
	state.execResultObserved = false
	state = resolveExecResult(state, lateSuccess, reader, time.Now().Add(time.Second))
	if state.launchPhase != launchEvidenceLost || state.launched() {
		t.Fatalf("late success upgraded launch state = %#v", state)
	}
}

func TestPidfdAuthenticationRejectsGenerationAndParentMismatch(t *testing.T) {
	identity := processIdentity{PID: 42, PPID: 7, StartTimeTicks: 100}
	if !pidfdAuthenticatesDirectChild(42, identity, 7) {
		t.Fatal("matching pidfd/direct-child evidence was rejected")
	}
	if pidfdAuthenticatesDirectChild(43, identity, 7) {
		t.Fatal("mismatched pidfd generation was accepted")
	}
	if pidfdAuthenticatesDirectChild(42, identity, 8) {
		t.Fatal("mismatched direct parent was accepted")
	}
}

func TestDescriptorContractRejectsSubstitutedCapabilities(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if err := validatePipeDescriptor(int(reader.Fd()), unix.O_RDONLY, "reader"); err != nil {
		t.Fatal(err)
	}
	if err := validatePipeDescriptor(int(writer.Fd()), unix.O_WRONLY, "writer"); err != nil {
		t.Fatal(err)
	}
	if err := validatePipeDescriptor(int(reader.Fd()), unix.O_WRONLY, "reader"); err == nil {
		t.Fatal("read end was accepted as a write capability")
	}
	regular, err := os.CreateTemp(t.TempDir(), "regular")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if err := validatePipeDescriptor(int(regular.Fd()), unix.O_RDWR, "regular"); err == nil {
		t.Fatal("regular file was accepted as a pipe capability")
	}
	if err := validatePathDescriptor(int(regular.Fd()), "regular"); err == nil {
		t.Fatal("read-write file was accepted as path-only authority")
	}
	if err := validateNullDescriptor(int(writer.Fd()), "writer"); err == nil {
		t.Fatal("pipe was accepted as the event placeholder")
	}
}

func TestDescriptorInheritanceIsExplicitlyFenced(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	for _, inherited := range []bool{false, true, false} {
		if err := setDescriptorInherited(int(writer.Fd()), inherited, "writer"); err != nil {
			t.Fatal(err)
		}
		flags, err := unix.FcntlInt(writer.Fd(), unix.F_GETFD, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := flags&unix.FD_CLOEXEC == 0; got != inherited {
			t.Fatalf("inherited = %t, want %t (flags %#x)", got, inherited, flags)
		}
	}
}

func TestPrivateGateDescriptorContractRejectsAmbientProcessDescriptors(t *testing.T) {
	if err := validateExecChildDescriptors(); err == nil {
		t.Fatal("ambient test-process descriptors satisfied the private exec-gate contract")
	}
	if err := prepareEventDescriptor(42); err == nil {
		t.Fatal("an unreserved test-event descriptor was accepted")
	}
}

func TestExecveatRejectsInvalidAuthorityAndStringVectors(t *testing.T) {
	if err := execveat(-1, []string{"target"}, []string{}); !errors.Is(err, unix.EBADF) {
		t.Fatalf("invalid execveat descriptor = %v", err)
	}
	if err := execveat(-1, []string{"target\x00argument"}, nil); err == nil {
		t.Fatal("execveat accepted an argument containing NUL")
	}
	if err := execveat(-1, []string{"target"}, []string{"NAME=value\x00suffix"}); err == nil {
		t.Fatal("execveat accepted an environment entry containing NUL")
	}
	if pointer := stringVectorPointer(nil); pointer != 0 {
		t.Fatalf("empty string-vector pointer = %#x", pointer)
	}
}

func TestExecGatePipesCloseEveryCapability(t *testing.T) {
	pipes, failureCode, err := openExecGatePipes()
	if err != nil || failureCode != "" {
		t.Fatalf("open exec-gate pipes: code=%q err=%v", failureCode, err)
	}
	if err := pipes.closeAll(); err != nil {
		t.Fatal(err)
	}
	for label, file := range map[string]*os.File{
		"child input reader": pipes.childInputReader,
		"child input writer": pipes.childInputWriter,
		"metadata reader":    pipes.metadataReader,
		"metadata writer":    pipes.metadataWriter,
		"ready reader":       pipes.readyReader,
		"ready writer":       pipes.readyWriter,
		"release reader":     pipes.releaseReader,
		"release writer":     pipes.releaseWriter,
		"result reader":      pipes.resultReader,
		"result writer":      pipes.resultWriter,
	} {
		if _, err := file.Stat(); err == nil {
			t.Fatalf("%s remained open", label)
		}
	}
}

func TestStreamChildInputEnforcesExactLength(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 32*1024+1)
	output, err := streamInputToPipe(bytes.NewReader(payload), &ownerprotocol.Stdin{ByteLength: int64(len(payload))})
	if err != nil || !bytes.Equal(output, payload) {
		t.Fatalf("stream = %d bytes, %v", len(output), err)
	}
	if output, err := streamInputToPipe(bytes.NewReader(nil), nil); err != nil || len(output) != 0 {
		t.Fatalf("empty stream = %q, %v", output, err)
	}
	if _, err := streamInputToPipe(strings.NewReader("short"), &ownerprotocol.Stdin{ByteLength: 6}); err == nil {
		t.Fatal("short input was accepted")
	}
	if output, err := streamInputToPipe(strings.NewReader("extra"), &ownerprotocol.Stdin{ByteLength: 4}); err == nil || string(output) != "extr" {
		t.Fatalf("extra input = %q, %v", output, err)
	}
}

func TestInputDeliveryRevocationJoinsBothSourceKinds(t *testing.T) {
	immediate := make(chan error, 1)
	immediate <- errLinuxInputFixture
	if err := awaitInputDelivery(bytes.NewReader(nil), immediate); !errors.Is(err, errLinuxInputFixture) {
		t.Fatalf("already-complete input delivery = %v", err)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	delivery := make(chan error, 1)
	go func() {
		_, readErr := reader.Read(make([]byte, 1))
		delivery <- readErr
	}()
	if err := awaitInputDelivery(reader, delivery); err == nil {
		t.Fatal("closing a blocked input source did not publish its terminal error")
	}

	deferred := make(chan error, 1)
	go func() { deferred <- errLinuxInputFixture }()
	if err := awaitInputDelivery(bytes.NewReader(nil), deferred); !errors.Is(err, errLinuxInputFixture) {
		t.Fatalf("non-closable input delivery = %v", err)
	}
}

func TestControlAndGateFraming(t *testing.T) {
	identity := ownerprotocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	var stop bytes.Buffer
	if err := ownerprotocol.WriteFrame(&stop, ownerprotocol.Control{
		SchemaVersion: ownerprotocol.ControlSchemaVersion, Identity: identity, Reason: ownerprotocol.ControlReasonStop,
	}); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		reader io.Reader
		want   string
	}{
		{reader: bytes.NewReader(stop.Bytes()), want: ownerprotocol.TerminationStop},
		{reader: bytes.NewReader(nil), want: ownerprotocol.TerminationParentLost},
		{reader: bytes.NewReader([]byte{1}), want: ownerprotocol.TerminationOwnerFailure},
	} {
		result := make(chan string, 1)
		watchControl(testCase.reader, identity, result)
		if got := <-result; got != testCase.want {
			t.Fatalf("control outcome = %q, want %q", got, testCase.want)
		}
	}
	if err := readExecGateReady(bytes.NewReader([]byte{1})); err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{nil, {0}} {
		if err := readExecGateReady(bytes.NewReader(encoded)); err == nil {
			t.Fatalf("readiness %v was accepted", encoded)
		}
	}
}

func TestEnvironmentAndDiagnostics(t *testing.T) {
	identity := ownerprotocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	environment := canonicalEnvironment([]ownerprotocol.EnvironmentEntry{
		{Name: "ALPHA", Value: "first"}, {Name: "ZETA", Value: "last"},
	}, 7, identity)
	want := []string{
		"ALPHA=first",
		"WINDSHARE_TEST_EVENT_FD=7",
		"WINDSHARE_TEST_OPERATION_ID=operation",
		"WINDSHARE_TEST_RUN_ID=run",
		"WINDSHARE_TEST_SCENARIO=scenario",
		"ZETA=last",
	}
	if !reflect.DeepEqual(environment, want) {
		t.Fatalf("environment = %q, want %q", environment, want)
	}
	if got := boundedDiagnostic(nil); got != "unknown linux process owner failure" {
		t.Fatalf("nil diagnostic = %q", got)
	}
	long := boundedDiagnostic(errors.New(strings.Repeat("x", maximumDiagnosticBytes+20)))
	if len(long) != maximumDiagnosticBytes {
		t.Fatalf("diagnostic length = %d", len(long))
	}
	if got := boundedDiagnostic(staticError(string([]byte{0xff, 'x'}))); !utf8.ValidString(got) {
		t.Fatalf("diagnostic is invalid UTF-8: %q", got)
	}
}

func TestInputAndCleanupEvidenceRemainOrthogonal(t *testing.T) {
	inputFailure := errors.New("input delivery failed")
	for _, testCase := range []struct {
		name      string
		authority *ownerprotocol.Stdin
		err       error
		want      string
	}{
		{name: "failure", authority: &ownerprotocol.Stdin{ByteLength: 1}, err: inputFailure, want: ownerprotocol.InputFailed},
		{name: "not requested", want: ownerprotocol.InputNotRequested},
		{name: "delivered", authority: &ownerprotocol.Stdin{ByteLength: 1}, want: ownerprotocol.InputDelivered},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			evidence := classifyInputEvidence(testCase.authority, testCase.err)
			if evidence.Outcome != testCase.want {
				t.Fatalf("input evidence = %#v", evidence)
			}
			if testCase.err != nil && (evidence.FailureCode == "" || evidence.FailureMessage == "") {
				t.Fatalf("input failure diagnostics = %#v", evidence)
			}
		})
	}

	authority := newInventoryAuthority(os.Getpid())
	authority.scans = 3
	authority.maximumDescendants = 2
	status := ownerprotocol.Settlement{}
	settleOwnershipEvidence(
		&status,
		authority,
		supervisionState{terminationReason: ownerprotocol.TerminationStop},
		false,
		nil,
	)
	if status.TerminationReason != ownerprotocol.TerminationStop || status.TreeState != ownerprotocol.TreeUnknown ||
		status.Cleanup.Outcome != ownerprotocol.CleanupFailed || status.Cleanup.FailureCode == "" ||
		status.Platform.ActiveProcessCount != nil || status.Platform.InventoryScans != 3 ||
		status.Platform.MaximumObservedDescendants != 2 {
		t.Fatalf("nonempty ownership evidence = %#v", status)
	}
}

func TestSettlementEvidence(t *testing.T) {
	identity := ownerprotocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	cause := errors.New("initialization failed")
	request := ownerprotocol.NewRequest(identity, ownerprotocol.Command{
		Executable: "/bin/true", Arguments: []string{}, WorkingDirectory: "/",
		Environment: []ownerprotocol.EnvironmentEntry{},
	}, 1_000, 100)
	settlement := failedSettlement(request, "FAILED", cause)
	if settlement.Target.FailureMessage != cause.Error() || settlement.TreeState != ownerprotocol.TreeProvenEmpty {
		t.Fatalf("failure settlement = %#v", settlement)
	}
	if err := validateSettlement(settlement, request); err != nil {
		t.Fatal(err)
	}
	evidence := terminalEvidence(nil, cause)
	if evidence.Outcome != ownerprotocol.TargetTerminalEvidenceLost || evidence.FailureMessage != cause.Error() {
		t.Fatalf("terminal evidence = %#v", evidence)
	}
}

func TestLaunchPreflightRejectsEachInvalidCapability(t *testing.T) {
	missingExecutable := ownerprotocol.Command{Executable: filepath.Join(t.TempDir(), "missing"), WorkingDirectory: t.TempDir()}
	decision := awaitLaunchDecision(missingExecutable, nil, nil)
	if decision.failure == nil || decision.failureCode != "EXECUTABLE_INVALID" || decision.authority != nil {
		t.Fatalf("missing executable decision = %#v", decision)
	}

	executable := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	decision = awaitLaunchDecision(ownerprotocol.Command{
		Executable: executable, WorkingDirectory: filepath.Join(t.TempDir(), "missing"),
	}, nil, nil)
	if decision.failure == nil || decision.failureCode != "WORKING_DIRECTORY_INVALID" || decision.authority != nil {
		t.Fatalf("missing working-directory decision = %#v", decision)
	}

	if err := Run(nil); err == nil {
		t.Fatal("public Linux owner entry point accepted an absent command")
	}
	if err := runSupervise([]string{"invalid"}); err == nil {
		t.Fatal("supervisor accepted its private command grammar")
	}
	var attempt bytes.Buffer
	if err := writeExecAttempt(&attempt); err != nil || !bytes.Equal(attempt.Bytes(), []byte{execAttemptMarker}) {
		t.Fatalf("exec-attempt marker = %v, %v", attempt.Bytes(), err)
	}
}

func TestLateExecutableAuthorityIsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	preflight := make(chan launchPreflight, 1)
	preflight <- launchPreflight{executable: &executableAuthority{file: file}}
	closeLateLaunchAuthority(preflight)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := file.Stat(); err != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("late executable authority remained open")
}

func TestWorkingDirectoryAuthorityPinsANoFollowDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "working")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	authority, err := holdWorkingDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.close()
	if err := authenticateHeldWorkingDirectory(authority.file); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := authenticateHeldWorkingDirectory(authority.file); err != nil {
		t.Fatalf("renamed directory capability was not retained: %v", err)
	}
	if _, err := holdWorkingDirectory(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing working directory was accepted")
	}
}

func streamInputToPipe(source io.Reader, authority *ownerprotocol.Stdin) ([]byte, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	type readResult struct {
		data []byte
		err  error
	}
	read := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		read <- readResult{data: data, err: err}
	}()
	streamErr := streamChildInput(source, writer, authority)
	result := <-read
	return result.data, errors.Join(streamErr, result.err)
}

type readerFunc func([]byte) (int, error)

func (read readerFunc) Read(buffer []byte) (int, error) { return read(buffer) }

type staticError string

func (message staticError) Error() string { return string(message) }

var errLinuxInputFixture = errors.New("input fixture terminal")
