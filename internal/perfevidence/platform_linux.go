//go:build linux

package perfevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unsafe"

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

// isReparsePointInfo is the no-follow boundary used by the evidence store.
// Linux exposes symbolic links through ModeSymlink rather than a reparse attribute.
func isReparsePointInfo(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink != 0
}
