//go:build linux

package mutationdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

const promotedFilesystemOverhead = int64(4 << 20)

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
