//go:build linux

package mutationdomain

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	helperImageDescriptor        = 3
	firstInputDescriptor         = helperImageDescriptor + 1
	targetTrampolineArgument     = "--perfevidence-mutation-target"
	secureNoRoot                 = 1 << 0
	secureNoRootLocked           = 1 << 1
	secureNoSetUIDFixup          = 1 << 2
	secureNoSetUIDFixupLocked    = 1 << 3
	secureKeepCapabilitiesLocked = 1 << 5
	secureNoAmbientRaise         = 1 << 6
	secureNoAmbientRaiseLocked   = 1 << 7
)

func maybeRunPlatformBroker([]string, io.Reader, io.Writer, io.Writer) (bool, int) {
	return false, 0
}

func maybeRunPlatformTarget(arguments []string, stderr io.Writer) (bool, int) {
	if len(arguments) < 2 || arguments[0] != targetTrampolineArgument {
		return false, 0
	}
	gate := os.NewFile(uintptr(helperImageDescriptor), "private-mutation-target-gate")
	if gate == nil {
		_, _ = fmt.Fprintln(stderr, "private mutation target gate is unavailable")
		return true, 1
	}
	var release [1]byte
	_, readErr := io.ReadFull(gate, release[:])
	gateCloseErr := gate.Close()
	if err := errors.Join(readErr, gateCloseErr); err != nil || release[0] != 1 {
		_, _ = fmt.Fprintf(stderr, "private mutation target gate failed: %v\n", err)
		return true, 1
	}
	if err := lockDownPlatformTarget(); err != nil {
		_, _ = fmt.Fprintf(stderr, "private mutation target privilege drop failed: %v\n", err)
		return true, 1
	}
	if err := unix.Exec(arguments[1], arguments[1:], os.Environ()); err != nil {
		_, _ = fmt.Fprintf(stderr, "private mutation target exec failed: %v\n", err)
		return true, 1
	}
	return true, 0
}

func platformTargetInvocation(executable string, arguments []string) (string, []string) {
	return "/proc/self/exe", append([]string{targetTrampolineArgument, executable}, arguments...)
}

func preparePlatformTarget(process *exec.Cmd) (
	afterStart func() error,
	release func() error,
	closeGate func() error,
	resultErr error,
) {
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	process.ExtraFiles = append(process.ExtraFiles, gateRead)
	afterStart = func() error {
		if gateRead == nil {
			return nil
		}
		err := gateRead.Close()
		gateRead = nil
		return err
	}
	release = func() error {
		if gateWrite == nil {
			return nil
		}
		written, writeErr := gateWrite.Write([]byte{1})
		closeErr := gateWrite.Close()
		gateWrite = nil
		if written != 1 && writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(writeErr, closeErr)
	}
	closeGate = func() error {
		var errs []error
		if gateRead != nil {
			errs = append(errs, gateRead.Close())
			gateRead = nil
		}
		if gateWrite != nil {
			errs = append(errs, gateWrite.Close())
			gateWrite = nil
		}
		return errors.Join(errs...)
	}
	return afterStart, release, closeGate, nil
}

func lockDownPlatformTarget() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges: %w", err)
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}
	secureBits := uintptr(
		secureNoRoot | secureNoRootLocked |
			secureNoSetUIDFixup | secureNoSetUIDFixupLocked |
			secureKeepCapabilitiesLocked |
			secureNoAmbientRaise | secureNoAmbientRaiseLocked,
	)
	if err := unix.Prctl(unix.PR_SET_SECUREBITS, secureBits, 0, 0, 0); err != nil {
		return fmt.Errorf("lock securebits: %w", err)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	capabilities := [2]unix.CapUserData{}
	if err := unix.Capset(&header, &capabilities[0]); err != nil {
		return fmt.Errorf("clear permitted/effective/inheritable capabilities: %w", err)
	}
	observed := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &observed[0]); err != nil {
		return fmt.Errorf("verify cleared capabilities: %w", err)
	}
	for _, set := range observed {
		if set.Effective != 0 || set.Permitted != 0 || set.Inheritable != 0 {
			return errors.New("target retained Linux capabilities after privilege drop")
		}
	}
	return nil
}

func openPlatformSession(ctx context.Context, configuration initialization) (*session, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	helper, err := os.Open(executable)
	if err != nil {
		return nil, err
	}
	if _, err := unix.FcntlInt(helper.Fd(), unix.F_SETLEASE, unix.F_RDLCK); err != nil {
		return nil, errors.Join(fmt.Errorf("retain private mutation helper image: %w", err), helper.Close())
	}
	var expected unix.Stat_t
	if err := unix.Fstat(int(helper.Fd()), &expected); err != nil {
		return nil, errors.Join(err, releaseHelperLease(helper), helper.Close())
	}
	rootFiles := make([]*os.File, 0, len(configuration.Roots))
	traversalBudget := productionMutationTraversalBudget()
	closeRootFiles := func() error {
		var errs []error
		for _, file := range rootFiles {
			if file != nil {
				errs = append(errs, file.Close())
			}
		}
		rootFiles = nil
		return errors.Join(errs...)
	}
	for index := range configuration.Roots {
		root := &configuration.Roots[index]
		if err := traversalBudget.admitCandidate(root.Name); err != nil {
			return nil, errors.Join(
				fmt.Errorf("admit private mutation input %s: %w", root.Name, err),
				closeRootFiles(), releaseHelperLease(helper), helper.Close(),
			)
		}
		fd, err := unix.Open(root.HostPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("retain private mutation input %s: %w", root.Name, err),
				closeRootFiles(), releaseHelperLease(helper), helper.Close(),
			)
		}
		file := os.NewFile(uintptr(fd), root.HostPath)
		identity, err := linuxTreeSHA256WithBudget(fd, traversalBudget)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("identify retained private mutation input %s: %w", root.Name, err),
				file.Close(), closeRootFiles(), releaseHelperLease(helper), helper.Close(),
			)
		}
		root.SHA256 = identity
		root.SourceDescriptor = firstInputDescriptor + index
		rootFiles = append(rootFiles, file)
	}
	command := exec.CommandContext(ctx, "/proc/self/fd/3", helperArgument)
	command.Env = []string{"LANG=C", "LC_ALL=C", helperRoleEnvironment + "=1"}
	command.ExtraFiles = append([]*os.File{helper}, rootFiles...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, errors.Join(err, closeRootFiles(), releaseHelperLease(helper), helper.Close())
	}
	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		return nil, errors.Join(err, stdin.Close(), closeRootFiles(), releaseHelperLease(helper), helper.Close())
	}
	stderr := &limitedBuffer{limit: maximumCapturedBytes}
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWPID,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
		Pdeathsig:                  syscall.SIGKILL,
	}
	if err := command.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("start retained user/mount/network/PID namespace helper: %w", err),
			stdin.Close(), closeRootFiles(), releaseHelperLease(helper), helper.Close(),
		)
	}
	rootCloseErr := closeRootFiles()
	fail := func(operationErr error) (*session, error) {
		killErr := command.Process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		cleanupErr := errors.Join(killErr, command.Wait(), stdin.Close(), releaseHelperLease(helper), helper.Close())
		stderrText := stderr.snapshot()
		var stderrErr error
		if len(stderrText) > 0 {
			stderrErr = errors.New(string(stderrText))
		}
		return nil, errors.Join(operationErr, stderrErr, cleanupErr)
	}
	if rootCloseErr != nil {
		return fail(rootCloseErr)
	}
	var observed unix.Stat_t
	if err := unix.Stat(filepath.Join("/proc", strconv.Itoa(command.Process.Pid), "exe"), &observed); err != nil ||
		observed.Dev != expected.Dev || observed.Ino != expected.Ino {
		return fail(errors.Join(errors.New("launched retained private mutation helper image was substituted"), err))
	}
	initializationSettled := make(chan struct{})
	cancelSettled := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = command.Process.Kill()
		close(cancelSettled)
	})
	defer func() {
		if stopCancellation() {
			close(cancelSettled)
		}
		<-cancelSettled
		close(initializationSettled)
	}()
	if err := writeJSONLine(stdin, configuration); err != nil {
		return fail(err)
	}
	reader := bufio.NewReaderSize(stdoutPipe, maximumProtocolLine)
	var ready response
	if err := readJSONLine(reader, &ready); err != nil {
		return fail(errors.Join(err, ctx.Err()))
	}
	if !stopCancellation() {
		<-cancelSettled
		return fail(errors.Join(context.Cause(ctx), errors.New("private mutation initialization was cancelled")))
	}
	close(cancelSettled)
	if ready.Error != "" {
		return fail(errors.New(ready.Error))
	}
	return &session{
		stdin: stdin, stdout: reader, stdoutPipe: stdoutPipe, stderr: stderr,
		kill: func() error {
			err := command.Process.Kill()
			if errors.Is(err, os.ErrProcessDone) {
				return nil
			}
			return err
		},
		wait: command.Wait,
		closePlatform: func() error {
			return errors.Join(releaseHelperLease(helper), helper.Close())
		},
		resolveProcessID: func(namespaceProcessID int) (int, error) {
			return resolvePlatformProcessID(command.Process.Pid, namespaceProcessID)
		},
	}, nil
}

func releaseHelperLease(file *os.File) error {
	if file == nil {
		return nil
	}
	_, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_UNLCK)
	return err
}
