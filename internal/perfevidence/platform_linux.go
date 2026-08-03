//go:build linux

package perfevidence

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/unix"
)

const (
	cleanupMutationLimit   = 64
	evidenceStoreReadBatch = 128
)

type outputRootAuthority struct {
	path     string
	identity directoryIdentity
	fd       int
}

type stageDirectoryAuthority struct {
	path       string
	name       string
	identity   directoryIdentity
	fd         int
	transition func(relative, phase string) error
	leaseHeld  bool
}

func evidenceFileClass(relative string) evidenceStoreFileClass {
	switch filepath.ToSlash(relative) {
	case manifestName, stageOwnerName:
		return evidenceMetadataFile
	case payloadName:
		return evidencePayloadFile
	default:
		return evidenceArtifactFile
	}
}

func defaultEvidenceStoreMeter() *evidenceStoreMeter {
	meter, err := newEvidenceStoreMeter(DefaultEvidenceStoreBudget())
	if err != nil {
		panic(err)
	}
	return meter
}

func (meter *evidenceStoreMeter) observeDirectory(relative string, depth int) error {
	return meter.observeObject(relative, depth, 0, evidenceArtifactFile)
}

func evidenceRelativeDepth(relative string) int {
	clean := filepath.ToSlash(filepath.Clean(relative))
	if clean == "." || clean == "" {
		return 1
	}
	return strings.Count(clean, "/") + 1
}

func requireDirectChildName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("filesystem authority child %q is not a direct name", name)
	}
	return nil
}

func (walk *evidenceStoreWalk) observeDirectory(relative string) error {
	if walk == nil || walk.meter == nil {
		return errors.New("evidence store walk has no shared meter")
	}
	return walk.meter.observeDirectory(relative, evidenceRelativeDepth(relative))
}

func (walk *evidenceStoreWalk) observeFile(relative string, file *os.File, info os.FileInfo) error {
	if walk == nil || walk.meter == nil {
		return errors.New("evidence store walk has no shared meter")
	}
	if _, alreadyCounted := walk.skipFiles[relative]; !alreadyCounted {
		if err := walk.meter.observeFile(
			relative, evidenceRelativeDepth(relative), info.Size(), evidenceFileClass(relative),
		); err != nil {
			return err
		}
	}
	if walk.visitor != nil {
		return walk.visitor(relative, file, info)
	}
	return nil
}

type protectedLinuxFile struct {
	file     *os.File
	path     string
	identity directoryIdentity
}

type linuxConsumptionAuthority struct {
	mu                      sync.Mutex
	inotifyFD               int
	files                   []protectedLinuxFile
	watchPaths              map[int32]string
	publicationRootWatch    int32
	publicationMoveExpected bool
	invalid                 error
	closed                  bool
	retainedFDs             []int
}

type linuxMutationOutput struct {
	path      string
	name      string
	parentFD  int
	inotifyFD int
	writer    *os.File
	hash      hash.Hash
	written   int64
	sealed    bool
	adopted   bool
	finalized bool
	linked    bool
	retained  *linuxConsumptionAuthority
}

func prepareMutationOutput(path string) (mutationOutputSink, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("disable parent ptrace attachment: %w", err)
	}
	parentPath := filepath.Dir(absolute)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(absolute)
	if err := requireDirectChildName(name); err != nil {
		return nil, errors.Join(err, unix.Close(parentFD))
	}
	var existing unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return nil, errors.Join(fmt.Errorf("protected output %s already exists", absolute), unix.Close(parentFD))
	} else if !errors.Is(err, unix.ENOENT) {
		return nil, errors.Join(err, unix.Close(parentFD))
	}
	inotifyFD, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, errors.Join(err, unix.Close(parentFD))
	}
	// The output is an unnamed inode until publication. Byte-write events for
	// that private inode are expected; only namespace and parent-identity events
	// can change which object the requested output path denotes.
	const mutationMask = unix.IN_ATTRIB | unix.IN_CREATE | unix.IN_DELETE |
		unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_MOVED_FROM |
		unix.IN_MOVED_TO | unix.IN_UNMOUNT
	if _, err := unix.InotifyAddWatch(inotifyFD, parentPath, mutationMask); err != nil {
		return nil, errors.Join(err, unix.Close(inotifyFD), unix.Close(parentFD))
	}
	temporaryFD, err := unix.Openat(
		parentFD, ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600,
	)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("create unnamed protected output (filesystem must support O_TMPFILE): %w", err),
			unix.Close(inotifyFD), unix.Close(parentFD),
		)
	}
	return &linuxMutationOutput{
		path: absolute, name: name, parentFD: parentFD, inotifyFD: inotifyFD,
		writer: os.NewFile(uintptr(temporaryFD), absolute), hash: sha256.New(),
	}, nil
}

func (output *linuxMutationOutput) WriteContext(ctx context.Context, content []byte) (int, error) {
	if output == nil || output.writer == nil || output.sealed || output.finalized {
		return 0, errors.New("protected output writer is closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, context.Cause(ctx)
	}
	written, err := output.writer.Write(content)
	if written > 0 {
		_, _ = output.hash.Write(content[:written])
		output.written += int64(written)
	}
	return written, errors.Join(err, context.Cause(ctx))
}

func (output *linuxMutationOutput) Seal(
	ctx context.Context,
	expectedBytes int64,
	expectedSHA256 string,
) (resultErr error) {
	if output == nil || output.writer == nil || output.sealed || output.finalized {
		return errors.New("protected output writer cannot be sealed")
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	observedSHA256 := hex.EncodeToString(output.hash.Sum(nil))
	if output.written != expectedBytes || observedSHA256 != expectedSHA256 {
		return fmt.Errorf(
			"protected output frame identity mismatch: got %d/%s, want %d/%s",
			output.written, observedSHA256, expectedBytes, expectedSHA256,
		)
	}
	if err := output.writer.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	readFD, err := unix.Open(
		fmt.Sprintf("/proc/self/fd/%d", output.writer.Fd()),
		unix.O_RDONLY|unix.O_CLOEXEC, 0,
	)
	if err != nil {
		return fmt.Errorf("reopen unnamed output read-only: %w", err)
	}
	readFile := os.NewFile(uintptr(readFD), output.path)
	if err := output.writer.Close(); err != nil {
		return errors.Join(err, readFile.Close())
	}
	output.writer = nil
	if _, err := unix.FcntlInt(readFile.Fd(), unix.F_SETLEASE, unix.F_RDLCK); err != nil {
		return errors.Join(fmt.Errorf("lease unnamed protected output: %w", err), readFile.Close())
	}
	procPath := fmt.Sprintf("/proc/self/fd/%d", readFile.Fd())
	if err := unix.Linkat(unix.AT_FDCWD, procPath, output.parentFD, output.name, unix.AT_SYMLINK_FOLLOW); err != nil {
		return errors.Join(
			fmt.Errorf("link leased output into the artifact namespace: %w", err),
			releaseLinuxLease(readFile), readFile.Close(),
		)
	}
	output.linked = true
	if err := requireOnlyLinuxOutputCreationEvent(output.inotifyFD, output.name); err != nil {
		return errors.Join(err, releaseLinuxLease(readFile), readFile.Close())
	}
	var stat unix.Stat_t
	if err := unix.Fstat(readFD, &stat); err != nil {
		return errors.Join(err, releaseLinuxLease(readFile), readFile.Close())
	}
	authority := &linuxConsumptionAuthority{
		inotifyFD: output.inotifyFD,
		files: []protectedLinuxFile{{
			file: readFile, path: output.path,
			identity: directoryIdentity{volume: uint64(stat.Dev), object: stat.Ino},
		}},
	}
	output.inotifyFD = -1
	if err := authority.Verify(); err != nil {
		return errors.Join(err, authority.closeWithoutVerify())
	}
	output.retained = authority
	output.sealed = true
	return nil
}

func requireOnlyLinuxOutputCreationEvent(inotifyFD int, name string) error {
	buffer := make([]byte, 64*1024)
	created := false
	for {
		count, err := unix.Read(inotifyFD, buffer)
		if errors.Is(err, unix.EAGAIN) {
			break
		}
		if err != nil {
			return err
		}
		for offset := 0; offset < count; {
			event := (*unix.InotifyEvent)(unsafe.Pointer(&buffer[offset]))
			offset += unix.SizeofInotifyEvent
			eventName := strings.TrimRight(string(buffer[offset:offset+int(event.Len)]), "\x00")
			offset += int(event.Len)
			if eventName != name || event.Mask != unix.IN_CREATE || created {
				return fmt.Errorf(
					"unexpected mutation while protected output %s was linked: name=%q mask=%#x duplicate_create=%t",
					name,
					eventName,
					event.Mask,
					created,
				)
			}
			created = true
		}
	}
	if !created {
		return fmt.Errorf("protected output %s was linked without an observable namespace event", name)
	}
	return nil
}

func (output *linuxMutationOutput) adopt() (byteConsumptionAuthority, error) {
	if output == nil || !output.sealed || output.adopted || output.finalized || output.retained == nil {
		return nil, errors.New("protected output is not a sealed, unadopted transaction")
	}
	output.adopted = true
	return output.retained, nil
}

func (output *linuxMutationOutput) finalize() {
	if output == nil || !output.sealed || !output.adopted || output.finalized || output.retained == nil {
		return
	}
	output.retained.mu.Lock()
	output.retained.retainedFDs = append(output.retained.retainedFDs, output.parentFD)
	output.retained.mu.Unlock()
	output.parentFD = -1
	output.finalized = true
}

func (output *linuxMutationOutput) Abort(ctx context.Context) error {
	if output == nil || output.finalized {
		return nil
	}
	var errs []error
	if output.writer != nil {
		errs = append(errs, output.writer.Close())
		output.writer = nil
	}
	if output.linked && output.parentFD >= 0 {
		errs = append(errs, unix.Unlinkat(output.parentFD, output.name, 0))
		output.linked = false
	}
	if output.retained != nil {
		errs = append(errs, output.retained.closeWithoutVerify())
		output.retained = nil
	}
	if output.inotifyFD >= 0 {
		errs = append(errs, unix.Close(output.inotifyFD))
		output.inotifyFD = -1
	}
	if output.parentFD >= 0 {
		errs = append(errs, unix.Close(output.parentFD))
		output.parentFD = -1
	}
	output.sealed = false
	output.adopted = false
	return errors.Join(errors.Join(errs...), context.Cause(ctx))
}

func acquireConsumptionAuthority(
	targets []snapshotValidationTarget,
	roots []string,
) (byteConsumptionAuthority, error) {
	if len(targets) == 0 || len(roots) == 0 {
		return nil, errors.New("consumption authority requires byte targets and directory roots")
	}
	inotifyFD, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("open monotonic mutation ledger: %w", err)
	}
	authority := &linuxConsumptionAuthority{inotifyFD: inotifyFD, watchPaths: make(map[int32]string)}
	fail := func(operationErr error) (byteConsumptionAuthority, error) {
		return nil, errors.Join(operationErr, authority.closeWithoutVerify())
	}
	directories, err := linuxAuthorityDirectories(targets, roots)
	if err != nil {
		return fail(err)
	}
	const mutationMask = unix.IN_ATTRIB | unix.IN_CLOSE_WRITE | unix.IN_CREATE | unix.IN_DELETE |
		unix.IN_DELETE_SELF | unix.IN_MODIFY | unix.IN_MOVE_SELF | unix.IN_MOVED_FROM |
		unix.IN_MOVED_TO | unix.IN_UNMOUNT
	for _, directory := range directories {
		watch, err := unix.InotifyAddWatch(inotifyFD, directory, mutationMask)
		if err != nil {
			return fail(fmt.Errorf("watch consumption directory %s: %w", directory, err))
		}
		authority.watchPaths[int32(watch)] = filepath.Clean(directory)
	}
	for _, target := range targets {
		fd, err := unix.Open(target.PhysicalPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fail(fmt.Errorf("open consumption byte %s: %w", target.LogicalPath, err))
		}
		file := os.NewFile(uintptr(fd), target.LogicalPath)
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != target.Bytes {
			return fail(errors.Join(
				fmt.Errorf("identify consumption byte %s", target.LogicalPath), statErr, file.Close(),
			))
		}
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_RDLCK); err != nil {
			return fail(errors.Join(
				fmt.Errorf("acquire kernel read lease for %s: %w", target.LogicalPath, err), file.Close(),
			))
		}
		identity := ArtifactFile{Path: target.LogicalPath, Bytes: target.Bytes, SHA256: target.SHA256}
		observed, hashErr := artifactIdentityFromOpenFile(file, info, target.LogicalPath)
		if hashErr != nil || observed != identity {
			return fail(errors.Join(
				fmt.Errorf("consumption byte %s changed while authority was acquired", target.LogicalPath),
				hashErr, releaseLinuxLease(file), file.Close(),
			))
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return fail(errors.Join(err, releaseLinuxLease(file), file.Close()))
		}
		authority.files = append(authority.files, protectedLinuxFile{
			file: file, path: target.PhysicalPath,
			identity: directoryIdentity{volume: uint64(stat.Dev), object: stat.Ino},
		})
	}
	if err := authority.Verify(); err != nil {
		return fail(err)
	}
	return authority, nil
}

func linuxAuthorityDirectories(
	targets []snapshotValidationTarget,
	roots []string,
) ([]string, error) {
	directories := make(map[string]struct{})
	cleanRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || isReparsePointInfo(info) {
			return nil, errors.Join(fmt.Errorf("consumption root %s is not a real directory", root), err)
		}
		clean := filepath.Clean(root)
		cleanRoots = append(cleanRoots, clean)
		directories[clean] = struct{}{}
	}
	for _, target := range targets {
		current := filepath.Dir(target.PhysicalPath)
		matched := false
		for _, root := range cleanRoots {
			if _, inside := relativeWithin(root, current); !inside {
				continue
			}
			matched = true
			for {
				directories[current] = struct{}{}
				if samePath(current, root) {
					break
				}
				parent := filepath.Dir(current)
				if samePath(parent, current) {
					return nil, fmt.Errorf("consumption target %s escaped authority root %s", target.LogicalPath, root)
				}
				current = parent
			}
			break
		}
		if !matched {
			return nil, fmt.Errorf("consumption target %s is outside every authority root", target.LogicalPath)
		}
	}
	result := make([]string, 0, len(directories))
	for directory := range directories {
		result = append(result, directory)
	}
	sort.Strings(result)
	return result, nil
}

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

func (stage *stageDirectoryAuthority) walkEvidenceStore(walk *evidenceStoreWalk) error {
	if stage == nil || stage.fd < 0 {
		return errors.New("stage directory authority is closed")
	}
	return walkLinuxRegularFiles(stage.fd, "", walk, stage.transition)
}

func walkLinuxRegularFiles(
	fd int,
	relative string,
	walk *evidenceStoreWalk,
	transition func(string, string) error,
) error {
	entries, err := readLinuxDirectory(fd, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		var before unix.Stat_t
		if err := unix.Fstatat(fd, entry.Name(), &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch before.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := walk.observeDirectory(childRelative); err != nil {
				return err
			}
			child, err := openLinuxChild(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
			if err != nil {
				return err
			}
			walkErr := requireLinuxHandleIdentity(child, before)
			if walkErr == nil && transition != nil {
				walkErr = transition(childRelative, "directory-opened")
			}
			if walkErr == nil {
				walkErr = walkLinuxRegularFiles(child, childRelative, walk, transition)
			}
			closeErr := unix.Close(child)
			if err := errors.Join(walkErr, closeErr, verifyLinuxEntryIdentity(fd, entry.Name(), before)); err != nil {
				return err
			}
		case unix.S_IFREG:
			child, err := openLinuxChild(fd, entry.Name(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
			if err != nil {
				return err
			}
			file := os.NewFile(uintptr(child), childRelative)
			info, statErr := file.Stat()
			visitErr := statErr
			if visitErr == nil {
				visitErr = requireLinuxHandleIdentity(child, before)
			}
			if visitErr == nil {
				if transition != nil {
					visitErr = transition(childRelative, "file-opened")
				}
			}
			if visitErr == nil {
				visitErr = walk.observeFile(childRelative, file, info)
			}
			closeErr := file.Close()
			if err := errors.Join(visitErr, closeErr, verifyLinuxEntryIdentity(fd, entry.Name(), before)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("artifact %s is a symlink or unsupported filesystem object", childRelative)
		}
	}
	return nil
}

func (stage *stageDirectoryAuthority) syncContents() error {
	if stage == nil || stage.fd < 0 {
		return errors.New("stage directory authority is closed")
	}
	return syncLinuxDirectoryContents(stage.fd, "", stage.transition, defaultEvidenceStoreMeter())
}

func syncLinuxDirectoryContents(
	fd int,
	relative string,
	transition func(string, string) error,
	meter *evidenceStoreMeter,
) error {
	entries, err := readLinuxDirectory(fd, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		var before unix.Stat_t
		if err := unix.Fstatat(fd, entry.Name(), &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch before.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := meter.observeDirectory(childRelative, evidenceRelativeDepth(childRelative)); err != nil {
				return err
			}
			child, err := openLinuxChild(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
			if err != nil {
				return err
			}
			syncErr := requireLinuxHandleIdentity(child, before)
			if syncErr == nil && transition != nil {
				syncErr = transition(childRelative, "directory-opened")
			}
			if syncErr == nil {
				syncErr = syncLinuxDirectoryContents(child, childRelative, transition, meter)
			}
			closeErr := unix.Close(child)
			if err := errors.Join(syncErr, closeErr, verifyLinuxEntryIdentity(fd, entry.Name(), before)); err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := meter.observeFile(
				childRelative, evidenceRelativeDepth(childRelative), before.Size, evidenceArtifactFile,
			); err != nil {
				return err
			}
			if transition != nil {
				if err := transition(childRelative, "file-opened"); err != nil {
					return err
				}
			}
			if err := syncLinuxRegularFile(fd, entry.Name(), before); err != nil {
				return err
			}
		default:
			return fmt.Errorf("refusing to sync symlink or unsupported artifact %s", childRelative)
		}
	}
	return unix.Fsync(fd)
}

func syncLinuxRegularFile(parent int, name string, before unix.Stat_t) error {
	readFD, err := openLinuxChild(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
	if err != nil {
		return err
	}
	if err := requireLinuxHandleIdentity(readFD, before); err != nil {
		return errors.Join(err, unix.Close(readFD))
	}
	originalMode := before.Mode & 0o7777
	if err := unix.Fchmod(readFD, originalMode|0o200); err != nil {
		return errors.Join(err, unix.Close(readFD))
	}
	writeFD, openErr := openLinuxChild(parent, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW)
	if openErr == nil {
		openErr = requireLinuxHandleIdentity(writeFD, before)
	}
	var syncErr, writeCloseErr error
	if openErr == nil {
		syncErr = unix.Fsync(writeFD)
	}
	if writeFD >= 0 {
		writeCloseErr = unix.Close(writeFD)
	}
	restoreErr := unix.Fchmod(readFD, originalMode)
	readCloseErr := unix.Close(readFD)
	return errors.Join(
		openErr, syncErr, writeCloseErr, restoreErr, readCloseErr,
		verifyLinuxEntryIdentity(parent, name, before),
	)
}

func openLinuxChild(parent int, name string, flags int) (int, error) {
	how := unix.OpenHow{
		Flags:   uint64(flags),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	}
	return unix.Openat2(parent, name, &how)
}

func authorityChildAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT)
}

func requireLinuxHandleIdentity(fd int, expected unix.Stat_t) error {
	var observed unix.Stat_t
	if err := unix.Fstat(fd, &observed); err != nil {
		return err
	}
	if observed.Dev != expected.Dev || observed.Ino != expected.Ino || observed.Mode&unix.S_IFMT != expected.Mode&unix.S_IFMT {
		return errors.New("filesystem entry changed between no-follow inspection and open")
	}
	return nil
}

func verifyLinuxEntryIdentity(parent int, name string, expected unix.Stat_t) error {
	var observed unix.Stat_t
	if err := unix.Fstatat(parent, name, &observed, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if observed.Dev != expected.Dev || observed.Ino != expected.Ino || observed.Mode&unix.S_IFMT != expected.Mode&unix.S_IFMT {
		return errors.New("filesystem entry changed during handle-relative traversal")
	}
	return nil
}

func (authority *outputRootAuthority) readDir() ([]os.DirEntry, error) {
	if err := authority.verifyPath(); err != nil {
		return nil, err
	}
	entries, err := readLinuxDirectory(authority.fd, authority.path)
	if err != nil {
		return nil, err
	}
	meter := defaultEvidenceStoreMeter()
	if err := meter.observeRootEntries(len(entries)); err != nil {
		return nil, err
	}
	return entries, nil
}

func (authority *outputRootAuthority) removeChild(name string, transition func(string) error) error {
	if authority == nil || authority.fd < 0 {
		return errors.New("evidence output authority is closed")
	}
	return removeLinuxEntryAt(authority.fd, name, name, transition)
}

func removeLinuxEntryAt(parent int, name, relative string, transition func(string) error) error {
	for range cleanupMutationLimit {
		var stat unix.Stat_t
		err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := unix.Unlinkat(parent, name, 0); errors.Is(err, unix.ENOENT) {
				return nil
			} else if err != nil {
				continue
			}
			return nil
		}
		child, err := openLinuxChild(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			continue
		}
		if err != nil {
			return err
		}
		if transition != nil {
			if err := transition(relative); err != nil {
				return errors.Join(err, unix.Close(child))
			}
		}
		emptyErr := emptyLinuxDirectory(child, relative, transition)
		closeErr := unix.Close(child)
		if err := errors.Join(emptyErr, closeErr); err != nil {
			return err
		}
		err = unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
		if err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.ENOTDIR) && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	return fmt.Errorf("directory entry %s kept changing during handle-relative cleanup", relative)
}

func emptyLinuxDirectory(fd int, relative string, transition func(string) error) error {
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return err
	}
	for range cleanupMutationLimit {
		entries, err := readLinuxDirectory(fd, relative)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for _, entry := range entries {
			child := filepath.ToSlash(filepath.Join(relative, entry.Name()))
			if err := removeLinuxEntryAt(fd, entry.Name(), child, transition); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("directory %s did not become empty during handle-relative cleanup", relative)
}

func readLinuxDirectory(fd int, name string) ([]os.DirEntry, error) {
	duplicate, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), name)
	maximumEntries := DefaultEvidenceStoreBudget().MaxObjects
	var entries []os.DirEntry
	var readErr error
	for {
		batch, err := file.ReadDir(evidenceStoreReadBatch)
		if len(batch) > maximumEntries-len(entries) {
			readErr = fmt.Errorf("evidence directory %s exceeds %d entries", name, maximumEntries)
			break
		}
		entries = append(entries, batch...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	return entries, errors.Join(readErr, file.Close())
}

func platformPathKey(path string) string {
	return filepath.Clean(path)
}

func platformPathAlias(_, _ string) bool {
	return false
}

func physicalMemory() (bytes uint64, probe string, resultErr error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, "/proc/meminfo", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] != "MemTotal:" || fields[2] != "kB" {
			continue
		}
		kilobytes, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil || kilobytes == 0 {
			return 0, "/proc/meminfo", errors.New("MemTotal was invalid")
		}
		return kilobytes * 1024, "/proc/meminfo", nil
	}
	if err := scanner.Err(); err != nil {
		return 0, "/proc/meminfo", err
	}
	return 0, "/proc/meminfo", errors.New("MemTotal was absent")
}

func cpuModel() (model string, resultErr error) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if found && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", scanner.Err()
}

func osDescription() string {
	encoded, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for line := range strings.SplitSeq(string(encoded), "\n") {
		if value, found := strings.CutPrefix(line, "PRETTY_NAME="); found {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return "Linux"
}

func currentProcessToken() (string, error) {
	token, _, err := linuxProcessToken(os.Getpid())
	return token, err
}

func processMatches(processID int, token string) (bool, error) {
	if processID <= 0 {
		return false, nil
	}
	observed, state, err := linuxProcessToken(processID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state == "Z" {
		return false, nil
	}
	if err := unix.Kill(processID, 0); errors.Is(err, unix.ESRCH) {
		return false, nil
	} else if err != nil && !errors.Is(err, unix.EPERM) {
		return false, err
	}
	return observed == token, nil
}

func linuxProcessToken(processID int) (string, string, error) {
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", err
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(processID), "stat"))
	if err != nil {
		return "", "", err
	}
	closing := strings.LastIndexByte(string(stat), ')')
	if closing < 0 {
		return "", "", errors.New("process stat omitted command terminator")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	const startTimeIndexAfterCommand = 19
	if len(fields) <= startTimeIndexAfterCommand {
		return "", "", errors.New("process stat omitted start time")
	}
	state := fields[0]
	token := fmt.Sprintf("%s:%s", strings.TrimSpace(string(bootID)), fields[startTimeIndexAfterCommand])
	return token, state, nil
}

func (authority *outputRootAuthority) sync() error {
	if authority == nil || authority.fd < 0 {
		return errors.New("evidence output authority is closed")
	}
	return unix.Fsync(authority.fd)
}

func (authority *outputRootAuthority) renameChildNoReplace(
	stage *stageDirectoryAuthority,
	destination string,
) error {
	if authority == nil || authority.fd < 0 {
		return errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(destination); err != nil {
		return err
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	if stage.transition != nil {
		if err := stage.transition("", "rename-source-verified"); err != nil {
			return err
		}
	}
	return unix.Renameat2(authority.fd, stage.name, authority.fd, destination, unix.RENAME_NOREPLACE)
}
