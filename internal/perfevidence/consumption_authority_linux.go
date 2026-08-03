//go:build linux

package perfevidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/unix"
)

func (authority *linuxConsumptionAuthority) Verify() error {
	if authority == nil {
		return errors.New("consumption authority is nil")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return errors.New("consumption authority is closed")
	}
	if authority.invalid == nil {
		buffer := make([]byte, 64*1024)
		for {
			n, err := unix.Read(authority.inotifyFD, buffer)
			if errors.Is(err, unix.EAGAIN) {
				break
			}
			if err != nil {
				authority.invalid = fmt.Errorf("read monotonic mutation ledger: %w", err)
				break
			}
			if n > 0 {
				authority.observeMutationEvents(buffer[:n])
			}
		}
	}
	for _, protected := range authority.files {
		lease, err := unix.FcntlInt(protected.file.Fd(), unix.F_GETLEASE, 0)
		if err != nil || lease != unix.F_RDLCK {
			authority.invalid = errors.Join(
				authority.invalid,
				fmt.Errorf("kernel read lease for %s was broken: state=%d: %w", protected.path, lease, err),
			)
			continue
		}
		var stat unix.Stat_t
		if err := unix.Stat(protected.path, &stat); err != nil ||
			(directoryIdentity{volume: uint64(stat.Dev), object: stat.Ino}) != protected.identity {
			authority.invalid = errors.Join(
				authority.invalid, fmt.Errorf("consumption path %s no longer names its retained inode: %w", protected.path, err),
			)
		}
	}
	return authority.invalid
}

func (authority *linuxConsumptionAuthority) observeMutationEvents(buffer []byte) {
	for offset := 0; offset < len(buffer); {
		if len(buffer)-offset < unix.SizeofInotifyEvent {
			authority.invalid = errors.Join(authority.invalid, errors.New("truncated consumption mutation event"))
			return
		}
		event := (*unix.InotifyEvent)(unsafe.Pointer(&buffer[offset]))
		eventBytes := unix.SizeofInotifyEvent + int(event.Len)
		if eventBytes < unix.SizeofInotifyEvent || eventBytes > len(buffer)-offset {
			authority.invalid = errors.Join(authority.invalid, errors.New("invalid consumption mutation event length"))
			return
		}
		mask := uint32(event.Mask) &^ uint32(unix.IN_ISDIR)
		if authority.publicationMoveExpected && event.Wd == authority.publicationRootWatch && mask == unix.IN_MOVE_SELF {
			authority.publicationMoveExpected = false
		} else {
			authority.invalid = errors.Join(
				authority.invalid,
				fmt.Errorf("consumption path %s mutated after authority acquisition (mask=%#x)", authority.watchPaths[event.Wd], event.Mask),
			)
		}
		offset += eventBytes
	}
}

func (authority *linuxConsumptionAuthority) preparePublicationRename(source string) error {
	if err := authority.Verify(); err != nil {
		return err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return errors.New("consumption authority is closed")
	}
	for watch, path := range authority.watchPaths {
		if samePath(path, source) {
			authority.publicationRootWatch = watch
			authority.publicationMoveExpected = true
			return nil
		}
	}
	return errors.New("publication root is not covered by the mutation ledger")
}

func (authority *linuxConsumptionAuthority) completePublicationRename(string) error {
	if err := authority.Verify(); err != nil {
		return err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.publicationMoveExpected {
		return errors.New("publication rename did not produce the expected monotonic namespace event")
	}
	authority.publicationRootWatch = 0
	return nil
}

func (authority *linuxConsumptionAuthority) VerifyProcessStart(
	evidence protocol.StartEvidence,
	executable string,
) (bool, error) {
	if err := authority.Verify(); err != nil {
		return false, err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	var expected *protectedLinuxFile
	for index := range authority.files {
		if samePath(authority.files[index].path, executable) {
			expected = &authority.files[index]
			break
		}
	}
	if expected == nil {
		return false, nil
	}
	if evidence.Platform != protocol.PlatformLinuxSubreaper {
		return true, errors.New("contained process start evidence has the wrong platform")
	}
	expectedIdentity := protocol.NewObjectIdentity64(expected.identity.volume, expected.identity.object)
	if evidence.Executable != expectedIdentity {
		return true, errors.New("contained process start evidence differs from its retained executable authority")
	}
	expectedTicks, err := strconv.ParseUint(evidence.ProcessInstance, 10, 64)
	if err != nil {
		return true, fmt.Errorf("parse contained process instance: %w", err)
	}
	observedTicks, err := linuxProcessStartTicks(evidence.ProcessID)
	if err != nil {
		return true, fmt.Errorf("identify contained process instance: %w", err)
	}
	if observedTicks != expectedTicks {
		return true, errors.New("contained process instance differs from authenticated start evidence")
	}
	return true, nil
}

func linuxProcessStartTicks(processID int) (uint64, error) {
	encoded, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(processID), "stat"))
	if err != nil {
		return 0, err
	}
	closeIndex := strings.LastIndex(string(encoded), ") ")
	if closeIndex < 0 {
		return 0, errors.New("process stat has no command boundary")
	}
	fields := strings.Fields(string(encoded[closeIndex+2:]))
	const startTimeIndexAfterCommand = 19
	if len(fields) <= startTimeIndexAfterCommand {
		return 0, errors.New("process stat omits start-time ticks")
	}
	ticks, err := strconv.ParseUint(fields[startTimeIndexAfterCommand], 10, 64)
	if err != nil || ticks == 0 {
		return 0, errors.Join(errors.New("process stat start-time ticks are invalid"), err)
	}
	return ticks, nil
}

func (authority *linuxConsumptionAuthority) Close() error {
	if authority == nil {
		return nil
	}
	verifyErr := authority.Verify()
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return errors.Join(verifyErr, authority.closeWithoutVerifyLocked())
}

func (authority *linuxConsumptionAuthority) closeWithoutVerify() error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.closeWithoutVerifyLocked()
}

func (authority *linuxConsumptionAuthority) closeWithoutVerifyLocked() error {
	if authority.closed {
		return nil
	}
	authority.closed = true
	var errs []error
	for _, protected := range authority.files {
		errs = append(errs, releaseLinuxLease(protected.file), protected.file.Close())
	}
	authority.files = nil
	authority.watchPaths = nil
	if authority.inotifyFD >= 0 {
		errs = append(errs, unix.Close(authority.inotifyFD))
		authority.inotifyFD = -1
	}
	for _, fd := range authority.retainedFDs {
		errs = append(errs, unix.Close(fd))
	}
	authority.retainedFDs = nil
	return errors.Join(errs...)
}

func releaseLinuxLease(file *os.File) error {
	if file == nil {
		return nil
	}
	_, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_UNLCK)
	return err
}

func openOutputRootAuthority(path string) (*outputRootAuthority, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence output root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create evidence output root: %w", err)
	}
	resolved, err := resolveDirectoryAuthority(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence output authority: %w", err)
	}
	fd, err := unix.Open(resolved, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open evidence output authority: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(
			fmt.Errorf("identify evidence output authority: %w", err),
			unix.Close(fd),
		)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o022 != 0 {
		return nil, errors.Join(
			errors.New("evidence output root must be current-user-owned and not group/world writable"),
			unix.Close(fd),
		)
	}
	return &outputRootAuthority{
		path: resolved, identity: directoryIdentity{volume: uint64(stat.Dev), object: stat.Ino}, fd: fd,
	}, nil
}

func openTreeAuthority(path string) (*stageDirectoryAuthority, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	clean := filepath.Clean(absolute)
	procPrefix := fmt.Sprintf("/proc/%d/fd/", os.Getpid())
	if strings.HasPrefix(clean, procPrefix) && !strings.Contains(strings.TrimPrefix(clean, procPrefix), "/") {
		original, parseErr := strconv.Atoi(strings.TrimPrefix(clean, procPrefix))
		if parseErr != nil {
			return nil, parseErr
		}
		duplicate, duplicateErr := unix.FcntlInt(uintptr(original), unix.F_DUPFD_CLOEXEC, 0)
		if duplicateErr != nil {
			return nil, duplicateErr
		}
		return linuxStageAuthorityFromFD(clean, filepath.Base(clean), duplicate)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if isReparsePointInfo(info) || !info.IsDir() {
		return nil, fmt.Errorf("artifact tree %s is not a real directory", clean)
	}
	fd, err := unix.Open(clean, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return linuxStageAuthorityFromFD(clean, filepath.Base(clean), fd)
}

func linuxStageAuthorityFromFD(path, name string, fd int) (*stageDirectoryAuthority, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o022 != 0 {
		return nil, errors.Join(
			errors.New("artifact tree must be current-user-owned and not group/world writable"),
			unix.Close(fd),
		)
	}
	return &stageDirectoryAuthority{
		path: path, name: name,
		identity: directoryIdentity{volume: uint64(stat.Dev), object: stat.Ino}, fd: fd,
	}, nil
}

func directoryIdentityAt(path string) (directoryIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return directoryIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return directoryIdentity{}, fmt.Errorf("%s is not a directory", path)
	}
	return directoryIdentity{volume: uint64(stat.Dev), object: stat.Ino}, nil
}

func (authority *outputRootAuthority) verifyPath() error {
	if authority == nil || authority.fd < 0 {
		return errors.New("evidence output authority is closed")
	}
	identity, err := directoryIdentityAt(authority.path)
	if err != nil {
		return fmt.Errorf("reidentify evidence output path: %w", err)
	}
	if identity != authority.identity {
		return errors.New("evidence output path no longer names the retained directory authority")
	}
	return nil
}

func (authority *outputRootAuthority) close() error {
	if authority == nil || authority.fd < 0 {
		return nil
	}
	fd := authority.fd
	authority.fd = -1
	return unix.Close(fd)
}

func (authority *outputRootAuthority) createChildAuthority(name string) (*stageDirectoryAuthority, error) {
	if err := requireDirectChildName(name); err != nil {
		return nil, err
	}
	if err := authority.verifyPath(); err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(authority.fd, name, 0o700); err != nil {
		return nil, err
	}
	return authority.openChildAuthority(name)
}

func (authority *outputRootAuthority) openChildAuthority(name string) (*stageDirectoryAuthority, error) {
	if authority == nil || authority.fd < 0 {
		return nil, errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(name); err != nil {
		return nil, err
	}
	child, err := openLinuxChild(
		authority.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
	)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(child, &stat); err != nil {
		return nil, errors.Join(err, unix.Close(child))
	}
	return &stageDirectoryAuthority{
		path: fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), child),
		name: name, identity: directoryIdentity{volume: uint64(stat.Dev), object: stat.Ino}, fd: child,
	}, nil
}

func (authority *outputRootAuthority) openRecoveryChildAuthority(name string) (*stageDirectoryAuthority, error) {
	return authority.openChildAuthority(name)
}

func (stage *stageDirectoryAuthority) acquireLiveLease(*outputRootAuthority) error {
	if stage == nil || stage.fd < 0 {
		return errors.New("stage directory authority is closed")
	}
	if err := unix.Flock(stage.fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("acquire live-stage lease: %w", err)
	}
	stage.leaseHeld = true
	return nil
}

func (stage *stageDirectoryAuthority) tryAcquireRecoveryLease(*outputRootAuthority) (bool, error) {
	if stage == nil || stage.fd < 0 {
		return false, errors.New("stage directory authority is closed")
	}
	err := unix.Flock(stage.fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire recovery lease: %w", err)
	}
	stage.leaseHeld = true
	return true, nil
}

func (stage *stageDirectoryAuthority) modTime() (time.Time, error) {
	if stage == nil || stage.fd < 0 {
		return time.Time{}, errors.New("stage directory authority is closed")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(stage.fd, &stat); err != nil {
		return time.Time{}, err
	}
	return time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec), nil
}

func (authority *outputRootAuthority) removeRetainedChild(
	stage *stageDirectoryAuthority,
	transition func(string) error,
) error {
	if stage == nil || stage.fd < 0 || !stage.leaseHeld {
		return errors.New("recovery removal requires a retained leased child authority")
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	if err := emptyLinuxDirectory(stage.fd, stage.name, transition); err != nil {
		return err
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	return unix.Unlinkat(authority.fd, stage.name, unix.AT_REMOVEDIR)
}

func (stage *stageDirectoryAuthority) verifyName(authority *outputRootAuthority) error {
	if stage == nil || stage.fd < 0 {
		return errors.New("stage directory authority is closed")
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(authority.fd, stage.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	identity := directoryIdentity{volume: uint64(stat.Dev), object: stat.Ino}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || identity != stage.identity {
		return errors.New("stage name no longer identifies its retained directory")
	}
	return nil
}

func (stage *stageDirectoryAuthority) close() error {
	if stage == nil || stage.fd < 0 {
		return nil
	}
	fd := stage.fd
	stage.fd = -1
	var unlockErr error
	if stage.leaseHeld {
		unlockErr = unix.Flock(fd, unix.LOCK_UN)
		stage.leaseHeld = false
	}
	return errors.Join(unlockErr, unix.Close(fd))
}

func (stage *stageDirectoryAuthority) matchesAuthority(other *stageDirectoryAuthority) error {
	if stage == nil || other == nil || other.fd < 0 {
		return errors.New("cannot compare closed directory authorities")
	}
	if stage.identity != other.identity {
		return errors.New("published authority does not identify the retained stage directory")
	}
	return nil
}

func (stage *stageDirectoryAuthority) openRegularFile(name string) (*os.File, os.FileInfo, error) {
	if filepath.Base(name) != name {
		return nil, nil, fmt.Errorf("artifact filename %s is not root-relative", name)
	}
	fd, err := openLinuxChild(stage.fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("artifact %s is not a regular file", name)
		}
		return nil, nil, errors.Join(err, file.Close())
	}
	return file, info, nil
}

func (stage *stageDirectoryAuthority) walkRegularFiles(visitor regularFileVisitor) error {
	return stage.walkEvidenceStore(&evidenceStoreWalk{meter: defaultEvidenceStoreMeter(), visitor: visitor})
}
