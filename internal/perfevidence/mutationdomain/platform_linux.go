//go:build linux

package mutationdomain

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	privateTmpfsSize             = "64g"
	maximumNamespaceProcesses    = 65_536
	namespaceProcessBatchSize    = 256
	promotedFilesystemOverhead   = int64(4 << 20)
	namespaceSettlementTimeout   = 5 * time.Second
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

type platformOutputAuthority struct {
	fd     int
	device uint64
	inode  uint64
	closed bool
}

type platformPromotedInput struct {
	root   string
	file   *os.File
	closed bool
}

type linuxOutputRevision struct {
	device uint64
	inode  uint64
	mode   uint32
	links  uint64
	size   int64
	mtime  unix.Timespec
	ctime  unix.Timespec
}

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

func preparePlatformHelper(configuration initialization) (
	privateRoot string,
	sources map[string]string,
	cleanup func() error,
	resultErr error,
) {
	if os.Getpid() != 1 {
		return "", nil, nil, errors.New("private mutation helper is not PID 1 in its namespace")
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return "", nil, nil, fmt.Errorf("make private mutation helper non-dumpable: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return "", nil, nil, fmt.Errorf("make mount propagation private: %w", err)
	}
	privateRoot = filepath.Join(configuration.RuntimeRoot, privateRootDirectory)
	if err := os.Mkdir(privateRoot, 0o700); err != nil {
		return "", nil, nil, err
	}
	mountedRoot := false
	mountedInputs := false
	var mutableMounts []string
	fail := func(operationErr error) (string, map[string]string, func() error, error) {
		var cleanupErr error
		if mountedInputs {
			cleanupErr = errors.Join(cleanupErr, unix.Unmount(filepath.Join(privateRoot, privateInputDirectory), unix.MNT_DETACH))
		}
		for index := len(mutableMounts) - 1; index >= 0; index-- {
			cleanupErr = errors.Join(cleanupErr, unix.Unmount(mutableMounts[index], unix.MNT_DETACH))
		}
		if mountedRoot {
			cleanupErr = errors.Join(cleanupErr, unix.Unmount(privateRoot, unix.MNT_DETACH))
		}
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(privateRoot))
		return "", nil, nil, errors.Join(operationErr, cleanupErr)
	}
	if err := unix.Mount(
		"tmpfs", privateRoot, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV,
		"mode=0700,size="+privateTmpfsSize,
	); err != nil {
		return fail(fmt.Errorf("mount private tmpfs mutation root: %w", err))
	}
	mountedRoot = true
	for _, directory := range []string{
		privateInputDirectory, privateOutputDirectory, privateCacheDirectory,
		privateTemporaryDirectory, privatePromotedDirectory, "proc", "dev", ".old-root",
	} {
		if err := os.Mkdir(filepath.Join(privateRoot, directory), 0o700); err != nil {
			return fail(err)
		}
	}
	for _, directory := range []string{
		privateOutputDirectory, privateCacheDirectory, privateTemporaryDirectory, privatePromotedDirectory,
	} {
		path := filepath.Join(privateRoot, directory)
		if err := unix.Mount(
			"tmpfs", path, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV,
			"mode=0700,size="+privateTmpfsSize,
		); err != nil {
			return fail(fmt.Errorf("mount private mutable %s filesystem: %w", directory, err))
		}
		mutableMounts = append(mutableMounts, path)
	}
	inputRoot := filepath.Join(privateRoot, privateInputDirectory)
	if err := unix.Mount(
		"tmpfs", inputRoot, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV,
		"mode=0700,size="+privateTmpfsSize,
	); err != nil {
		return fail(fmt.Errorf("mount private immutable input filesystem: %w", err))
	}
	mountedInputs = true
	sources = make(map[string]string, len(configuration.Roots))
	traversalBudget := productionMutationTraversalBudget()
	for _, root := range configuration.Roots {
		if err := traversalBudget.admitCandidate(root.Name); err != nil {
			return fail(fmt.Errorf("admit retained private mutation input %s: %w", root.Name, err))
		}
		if root.SourceDescriptor < firstInputDescriptor {
			return fail(fmt.Errorf("private mutation input %s has no retained descriptor", root.Name))
		}
		var stat unix.Stat_t
		if err := unix.Fstat(root.SourceDescriptor, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fail(errors.Join(fmt.Errorf("retained private mutation input %s is not a directory", root.Name), err))
		}
		destination := filepath.Join(inputRoot, root.Name)
		identity, err := copyLinuxTreeWithBudget(root.SourceDescriptor, destination, traversalBudget)
		if err != nil || identity != root.SHA256 {
			return fail(errors.Join(
				fmt.Errorf("retained input root %s has identity %s, want %s", root.Name, identity, root.SHA256),
				err,
			))
		}
		sources[root.Name] = filepath.Join(string(filepath.Separator), privateInputDirectory, root.Name)
	}
	var closeErrs []error
	for _, root := range configuration.Roots {
		if err := unix.Close(root.SourceDescriptor); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close retained input %s: %w", root.Name, err))
		}
	}
	if err := unix.Close(helperImageDescriptor); err != nil {
		closeErrs = append(closeErrs, fmt.Errorf("close retained helper image: %w", err))
	}
	if err := errors.Join(closeErrs...); err != nil {
		return fail(err)
	}
	if err := unix.Mount(
		"", inputRoot, "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "",
	); err != nil {
		return fail(fmt.Errorf("seal private input filesystem read-only: %w", err))
	}
	for _, device := range []string{"null", "zero", "random", "urandom"} {
		target := filepath.Join(privateRoot, "dev", device)
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return fail(err)
		}
		if err := file.Close(); err != nil {
			return fail(err)
		}
		if err := unix.Mount(filepath.Join("/dev", device), target, "", unix.MS_BIND, ""); err != nil {
			return fail(fmt.Errorf("bind private device %s: %w", device, err))
		}
	}
	if err := os.Symlink("/proc/self/fd", filepath.Join(privateRoot, "dev", "fd")); err != nil {
		return fail(err)
	}
	if err := unix.Mount(
		"proc", filepath.Join(privateRoot, "proc"), "proc",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "hidepid=2",
	); err != nil {
		return fail(fmt.Errorf("mount private PID namespace procfs: %w", err))
	}
	if err := unix.PivotRoot(privateRoot, filepath.Join(privateRoot, ".old-root")); err != nil {
		return fail(fmt.Errorf("pivot private mutation filesystem root: %w", err))
	}
	if err := unix.Chdir("/"); err != nil {
		return "", nil, nil, err
	}
	if err := unix.Unmount("/.old-root", unix.MNT_DETACH); err != nil {
		return "", nil, nil, fmt.Errorf("detach former host filesystem root: %w", err)
	}
	if err := os.Remove("/.old-root"); err != nil {
		return "", nil, nil, fmt.Errorf("remove detached former host root: %w", err)
	}
	if err := unix.Mount(
		"", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "",
	); err != nil {
		return "", nil, nil, fmt.Errorf("seal private mutation root mount read-only: %w", err)
	}
	if err := remountPromotedParent(true); err != nil {
		return "", nil, nil, fmt.Errorf("seal private promotion authority read-only: %w", err)
	}
	cleanup = func() error {
		var errs []error
		for _, directory := range []string{
			privatePromotedDirectory, privateOutputDirectory, privateCacheDirectory, privateTemporaryDirectory,
		} {
			errs = append(errs, unix.Unmount("/"+directory, unix.MNT_DETACH))
		}
		return errors.Join(errs...)
	}
	return "/", sources, cleanup, nil
}

func helperTargetProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}

func resolvePlatformProcessID(helperHostID, namespaceProcessID int) (resultID int, resultErr error) {
	if helperHostID <= 0 || namespaceProcessID <= 1 {
		return 0, errors.New("Linux namespace process identity is invalid")
	}
	var helperNamespace unix.Stat_t
	if err := unix.Stat(filepath.Join("/proc", strconv.Itoa(helperHostID), "ns", "pid"), &helperNamespace); err != nil {
		return 0, fmt.Errorf("identify private mutation PID namespace: %w", err)
	}
	directory, err := os.Open("/proc")
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	visited := 0
	for {
		entries, readErr := directory.ReadDir(namespaceProcessBatchSize)
		for _, entry := range entries {
			hostID, parseErr := strconv.Atoi(entry.Name())
			if parseErr != nil || hostID <= 0 {
				continue
			}
			visited++
			if visited > maximumNamespaceProcesses {
				return 0, errors.New("host process table exceeded the private mutation identity bound")
			}
			matches, matchErr := linuxNamespaceProcessMatches(
				hostID, namespaceProcessID, uint64(helperNamespace.Dev), helperNamespace.Ino,
			)
			if matchErr != nil {
				if errors.Is(matchErr, os.ErrNotExist) || errors.Is(matchErr, unix.ESRCH) {
					continue
				}
				return 0, matchErr
			}
			if matches {
				if resultID != 0 {
					return 0, errors.New("private mutation namespace process identity is ambiguous")
				}
				resultID = hostID
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
	}
	if resultID == 0 {
		return 0, errors.New("private mutation target has no host PID mapping")
	}
	return resultID, nil
}

func linuxNamespaceProcessMatches(hostID, namespaceID int, namespaceDevice, namespaceInode uint64) (bool, error) {
	var namespace unix.Stat_t
	if err := unix.Stat(filepath.Join("/proc", strconv.Itoa(hostID), "ns", "pid"), &namespace); err != nil {
		return false, err
	}
	if uint64(namespace.Dev) != namespaceDevice || namespace.Ino != namespaceInode {
		return false, nil
	}
	status, err := os.Open(filepath.Join("/proc", strconv.Itoa(hostID), "status"))
	if err != nil {
		return false, err
	}
	content, readErr := io.ReadAll(io.LimitReader(status, maximumProtocolLine))
	closeErr := status.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "NSpid:" {
			continue
		}
		observed, err := strconv.Atoi(fields[len(fields)-1])
		return err == nil && observed == namespaceID, err
	}
	return false, errors.New("host process omitted its namespace PID identity")
}

func settlePlatformTarget() error {
	deadline := time.Now().Add(namespaceSettlementTimeout)
	for {
		if err := visitNamespaceProcesses(func(pid int) error {
			if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
		for {
			var status unix.WaitStatus
			pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if errors.Is(err, unix.ECHILD) || pid == 0 {
				break
			}
			if err != nil {
				return err
			}
		}
		remaining := 0
		if err := visitNamespaceProcesses(func(int) error {
			remaining++
			return nil
		}); err != nil {
			return err
		}
		if remaining == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("private mutation PID namespace retained %d target processes", remaining)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func visitNamespaceProcesses(visitor func(int) error) (resultErr error) {
	directory, err := os.Open("/proc")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	count := 0
	for {
		entries, readErr := directory.ReadDir(namespaceProcessBatchSize)
		for _, entry := range entries {
			pid, parseErr := strconv.Atoi(entry.Name())
			if parseErr != nil || pid <= 1 {
				continue
			}
			count++
			if count > maximumNamespaceProcesses {
				return errors.New("private mutation PID namespace exceeded its process bound")
			}
			if err := visitor(pid); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func openPlatformOutputAuthority(path string) (*platformOutputAuthority, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	var identity unix.Stat_t
	if err := unix.Fstat(fd, &identity); err != nil || identity.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, errors.Join(errors.New("isolated output authority is not a directory"), err, unix.Close(fd))
	}
	return &platformOutputAuthority{fd: fd, device: uint64(identity.Dev), inode: identity.Ino}, nil
}

func (authority *platformOutputAuthority) close() error {
	if authority == nil || authority.closed {
		return nil
	}
	authority.closed = true
	return unix.Close(authority.fd)
}

func (authority *platformOutputAuthority) verify() error {
	if authority == nil || authority.closed {
		return errors.New("isolated output directory authority is closed")
	}
	var observed unix.Stat_t
	if err := unix.Fstat(authority.fd, &observed); err != nil {
		return err
	}
	if observed.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(observed.Dev) != authority.device || observed.Ino != authority.inode {
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
	fd, err := unix.Openat(authority.fd, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	var initial unix.Stat_t
	if err := unix.Fstat(fd, &initial); err != nil || initial.Mode&unix.S_IFMT != unix.S_IFREG || initial.Nlink != 1 {
		return nil, nil, errors.Join(errors.New("isolated output is not a single-link no-follow regular file"), err, unix.Close(fd))
	}
	revision := linuxOutputRevision{
		device: uint64(initial.Dev), inode: initial.Ino, mode: initial.Mode, links: initial.Nlink,
		size: initial.Size, mtime: initial.Mtim, ctime: initial.Ctim,
	}
	verify := func() error {
		if err := authority.verify(); err != nil {
			return err
		}
		var handleRevision unix.Stat_t
		if err := unix.Fstat(fd, &handleRevision); err != nil {
			return err
		}
		observed := linuxOutputRevision{
			device: uint64(handleRevision.Dev), inode: handleRevision.Ino, mode: handleRevision.Mode,
			links: handleRevision.Nlink, size: handleRevision.Size,
			mtime: handleRevision.Mtim, ctime: handleRevision.Ctim,
		}
		if observed != revision {
			return errors.New("isolated output revision changed while framing")
		}
		var named unix.Stat_t
		if err := unix.Fstatat(authority.fd, leaf, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if named.Mode&unix.S_IFMT != unix.S_IFREG || uint64(named.Dev) != revision.device || named.Ino != revision.inode {
			return errors.New("isolated output name no longer identifies the framed file")
		}
		return nil
	}
	return os.NewFile(uintptr(fd), leaf), verify, nil
}

func platformPromoteProtectedOutput(
	source *os.File,
	verifySource func() error,
	expectedBytes int64,
	expectedSHA256 string,
	mode os.FileMode,
	semanticPath string,
) (*platformPromotedInput, error) {
	if source == nil || verifySource == nil || expectedBytes < 0 ||
		expectedBytes > maximumMutationInputBytes-promotedFilesystemOverhead {
		return nil, errors.New("Linux output promotion authority is invalid")
	}
	if digest, err := hex.DecodeString(expectedSHA256); err != nil || len(digest) != sha256.Size {
		return nil, errors.New("Linux output promotion digest is invalid")
	}
	if err := verifySource(); err != nil {
		return nil, fmt.Errorf("verify retained output before promotion: %w", err)
	}
	if err := remountPromotedParent(false); err != nil {
		return nil, fmt.Errorf("open private promotion authority for generation creation: %w", err)
	}
	parentWritable := true
	generationRoot, err := os.MkdirTemp("/"+privatePromotedDirectory, "generation-")
	if err != nil {
		return nil, errors.Join(err, remountPromotedParent(true))
	}
	generationMounted := false
	var retained *os.File
	fail := func(operationErr error) (*platformPromotedInput, error) {
		var cleanupErr error
		if retained != nil {
			cleanupErr = errors.Join(cleanupErr, retained.Close())
			retained = nil
		}
		if generationMounted {
			cleanupErr = errors.Join(cleanupErr, unix.Unmount(generationRoot, unix.MNT_DETACH))
			generationMounted = false
		}
		if !parentWritable {
			cleanupErr = errors.Join(cleanupErr, remountPromotedParent(false))
			parentWritable = true
		}
		cleanupErr = errors.Join(cleanupErr, os.Remove(generationRoot), remountPromotedParent(true))
		parentWritable = false
		return nil, errors.Join(operationErr, cleanupErr)
	}
	filesystemBytes := expectedBytes + promotedFilesystemOverhead
	if err := unix.Mount(
		"tmpfs", generationRoot, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV,
		"mode=0700,size="+strconv.FormatInt(filesystemBytes, 10),
	); err != nil {
		return fail(fmt.Errorf("mount private promoted output generation: %w", err))
	}
	generationMounted = true
	if err := remountPromotedParent(true); err != nil {
		return fail(fmt.Errorf("seal private promotion parent after generation creation: %w", err))
	}
	parentWritable = false
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	artifactPath := filepath.Join(generationRoot, promotedArtifactName(semanticPath))
	artifact, err := os.OpenFile(artifactPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(err)
	}
	hasher := sha256.New()
	written, copyErr := io.CopyN(io.MultiWriter(artifact, hasher), source, expectedBytes)
	var extra [1]byte
	extraBytes, extraErr := source.Read(extra[:])
	if errors.Is(extraErr, io.EOF) {
		extraErr = nil
	}
	modeErr := artifact.Chmod(mode.Perm())
	syncErr := artifact.Sync()
	information, statErr := artifact.Stat()
	closeErr := artifact.Close()
	observedSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if copyErr != nil || written != expectedBytes || extraErr != nil || extraBytes != 0 ||
		statErr != nil || information == nil || information.Size() != expectedBytes ||
		information.Mode().Perm() != mode.Perm() || observedSHA256 != expectedSHA256 {
		return fail(errors.Join(
			fmt.Errorf(
				"promoted Linux output identity is bytes=%d sha256=%s, want bytes=%d sha256=%s",
				written, observedSHA256, expectedBytes, expectedSHA256,
			),
			copyErr, extraErr, modeErr, syncErr, statErr, closeErr,
		))
	}
	if err := errors.Join(modeErr, syncErr, closeErr, verifySource()); err != nil {
		return fail(fmt.Errorf("settle retained Linux output promotion: %w", err))
	}
	if err := unix.Mount(
		"", generationRoot, "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "",
	); err != nil {
		return fail(fmt.Errorf("seal promoted Linux output generation read-only: %w", err))
	}
	fd, err := unix.Open(artifactPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fail(err)
	}
	retained = os.NewFile(uintptr(fd), artifactPath)
	var retainedIdentity unix.Stat_t
	if err := unix.Fstat(fd, &retainedIdentity); err != nil ||
		retainedIdentity.Mode&unix.S_IFMT != unix.S_IFREG || retainedIdentity.Nlink != 1 ||
		retainedIdentity.Size != expectedBytes || os.FileMode(retainedIdentity.Mode).Perm() != mode.Perm() {
		return fail(errors.Join(errors.New("promoted Linux output retained an unexpected identity"), err))
	}
	result := &platformPromotedInput{root: generationRoot, file: retained}
	retained = nil
	return result, nil
}

func remountPromotedParent(readOnly bool) error {
	flags := uintptr(unix.MS_REMOUNT | unix.MS_NOSUID | unix.MS_NODEV)
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	return unix.Mount("", "/"+privatePromotedDirectory, "", flags, "")
}

func (input *platformPromotedInput) path() string {
	if input == nil || input.file == nil {
		return ""
	}
	return input.file.Name()
}

func (input *platformPromotedInput) close() error {
	if input == nil || input.closed {
		return nil
	}
	input.closed = true
	var closeErr error
	if input.file != nil {
		closeErr = input.file.Close()
		input.file = nil
	}
	unmountErr := unix.Unmount(input.root, unix.MNT_DETACH)
	writableErr := remountPromotedParent(false)
	var removeErr error
	if writableErr == nil {
		removeErr = os.Remove(input.root)
	}
	resealErr := remountPromotedParent(true)
	return errors.Join(closeErr, unmountErr, writableErr, removeErr, resealErr)
}

func linuxTreeSHA256(rootFD int) (string, error) {
	budget := productionMutationTraversalBudget()
	if err := budget.admitCandidate("source"); err != nil {
		return "", err
	}
	return linuxTreeSHA256WithBudget(rootFD, budget)
}

func linuxTreeSHA256WithBudget(rootFD int, budget *mutationTraversalBudget) (string, error) {
	records, err := walkLinuxTree(rootFD, "", "", budget, 0)
	if err != nil {
		return "", err
	}
	sort.Strings(records)
	return hashBytes([]byte(strings.Join(records, "\n"))), nil
}

func copyLinuxTree(rootFD int, destination string) (string, error) {
	budget := productionMutationTraversalBudget()
	if err := budget.admitCandidate("source"); err != nil {
		return "", err
	}
	return copyLinuxTreeWithBudget(rootFD, destination, budget)
}

func copyLinuxTreeWithBudget(rootFD int, destination string, budget *mutationTraversalBudget) (string, error) {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return "", err
	}
	records, err := walkLinuxTree(rootFD, "", destination, budget, 0)
	if err != nil {
		return "", err
	}
	sort.Strings(records)
	return hashBytes([]byte(strings.Join(records, "\n"))), nil
}

func walkLinuxTree(
	directoryFD int,
	prefix string,
	destination string,
	budget *mutationTraversalBudget,
	depth int,
) (records []string, resultErr error) {
	// Reopening through the retained directory handle creates an independent
	// directory cursor. dup(2) would share the cursor and make the first identity
	// pass silently consume the entries needed by the later immutable copy.
	duplicate, err := unix.Openat(
		directoryFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(duplicate), prefix)
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	seen := make(map[string]struct{})
	for {
		entries, readErr := directory.ReadDir(mutationTreeBatchSize)
		sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
		for _, entry := range entries {
			name := entry.Name()
			if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
				return nil, fmt.Errorf("private mutation input contains invalid name %q", name)
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, fmt.Errorf("private mutation input enumeration repeated name %q", name)
			}
			seen[name] = struct{}{}
			relative := filepath.ToSlash(filepath.Join(prefix, name))
			var observed unix.Stat_t
			if err := unix.Fstatat(directoryFD, name, &observed, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return nil, err
			}
			switch observed.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				if err := budget.admitObject(relative, depth+1, 0); err != nil {
					return nil, err
				}
				childFD, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				if err != nil {
					return nil, err
				}
				var stable unix.Stat_t
				if err := unix.Fstat(childFD, &stable); err != nil || stable.Dev != observed.Dev || stable.Ino != observed.Ino ||
					stable.Mode&unix.S_IFMT != unix.S_IFDIR {
					return nil, errors.Join(errors.New("private mutation input directory changed during retained open"), err, unix.Close(childFD))
				}
				childDestination := ""
				if destination != "" {
					childDestination = filepath.Join(destination, name)
					expectedMode := uint32(observed.Mode & 0o777)
					if err := os.Mkdir(childDestination, 0o700); err != nil {
						_ = unix.Close(childFD)
						return nil, err
					}
					destinationFD, err := unix.Open(
						childDestination, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
					)
					if err != nil {
						_ = unix.Close(childFD)
						return nil, err
					}
					var copied unix.Stat_t
					modeErr := unix.Fchmod(destinationFD, expectedMode)
					statErr := unix.Fstat(destinationFD, &copied)
					closeErr := unix.Close(destinationFD)
					if err := errors.Join(modeErr, statErr, closeErr); err != nil || copied.Mode&0o777 != expectedMode {
						_ = unix.Close(childFD)
						return nil, errors.Join(fmt.Errorf("copied directory %s did not retain its admitted mode", relative), err)
					}
				}
				childRecords, walkErr := walkLinuxTree(childFD, relative, childDestination, budget, depth+1)
				closeErr := unix.Close(childFD)
				if err := errors.Join(walkErr, closeErr); err != nil {
					return nil, err
				}
				records = append(records, fmt.Sprintf("D\x00%s\x00%o", relative, observed.Mode&0o777))
				records = append(records, childRecords...)
			case unix.S_IFREG:
				if observed.Nlink != 1 {
					return nil, fmt.Errorf("private mutation input %s is not a single-link regular file", relative)
				}
				if err := budget.admitObject(relative, depth+1, observed.Size); err != nil {
					return nil, err
				}
				fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				if err != nil {
					return nil, err
				}
				var stable unix.Stat_t
				if err := unix.Fstat(fd, &stable); err != nil || stable.Dev != observed.Dev || stable.Ino != observed.Ino ||
					stable.Mode&unix.S_IFMT != unix.S_IFREG || stable.Nlink != 1 || stable.Size != observed.Size {
					return nil, errors.Join(errors.New("private mutation input changed during retained open"), err, unix.Close(fd))
				}
				file := os.NewFile(uintptr(fd), relative)
				hasher := sha256.New()
				writer := io.Writer(hasher)
				var output *os.File
				if destination != "" {
					output, err = os.OpenFile(
						filepath.Join(destination, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL,
						os.FileMode(stable.Mode&0o777),
					)
					if err != nil {
						return nil, errors.Join(err, file.Close())
					}
					writer = io.MultiWriter(hasher, output)
				}
				written, copyErr := io.CopyN(writer, file, stable.Size)
				var extra [1]byte
				extraBytes, extraErr := file.Read(extra[:])
				if errors.Is(extraErr, io.EOF) {
					extraErr = nil
				}
				var outputErr error
				if output != nil {
					expectedMode := os.FileMode(stable.Mode & 0o777)
					modeErr := output.Chmod(expectedMode)
					copied, statErr := output.Stat()
					if statErr == nil && copied.Mode().Perm() != expectedMode {
						statErr = fmt.Errorf("copied file %s did not retain its admitted mode", relative)
					}
					outputErr = errors.Join(modeErr, statErr, output.Sync(), output.Close())
				}
				if err := errors.Join(copyErr, extraErr, outputErr, file.Close()); err != nil ||
					written != stable.Size || extraBytes != 0 {
					return nil, errors.Join(fmt.Errorf("retained input %s changed while copying", relative), err)
				}
				records = append(records, fmt.Sprintf(
					"F\x00%s\x00%d\x00%o\x00%s", relative, stable.Size, stable.Mode&0o777,
					hex.EncodeToString(hasher.Sum(nil)),
				))
			default:
				if err := budget.admitObject(relative, depth+1, 0); err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("private mutation input contains unsupported object %s", relative)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return records, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func releaseHelperLease(file *os.File) error {
	if file == nil {
		return nil
	}
	_, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_UNLCK)
	return err
}
