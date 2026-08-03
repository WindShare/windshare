//go:build linux

package mutationdomain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	privateTmpfsSize           = "64g"
	maximumNamespaceProcesses  = 65_536
	namespaceProcessBatchSize  = 256
	namespaceSettlementTimeout = 5 * time.Second
)

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
	for line := range strings.SplitSeq(string(content), "\n") {
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
