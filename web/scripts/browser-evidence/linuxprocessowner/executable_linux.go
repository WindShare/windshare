//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"syscall"
)

const (
	maximumExecutableBytes     = 512 << 20
	targetExecutableDescriptor = 6
)

type executablePreflight struct {
	authority *executableAuthority
	err       error
}

type executableAuthority struct {
	file       *os.File
	path       string
	identity   os.FileInfo
	byteLength int64
	sha256     string
}

func runExecChild() error {
	metadata := os.NewFile(3, "exec-gate-metadata")
	ready := os.NewFile(4, "exec-gate-ready")
	release := os.NewFile(5, "exec-gate-release")
	target := os.NewFile(targetExecutableDescriptor, "exec-gate-target")
	if metadata == nil || ready == nil || release == nil || target == nil {
		return errors.New("exec gate requires metadata, ready, release, and target descriptors")
	}
	defer metadata.Close()
	defer ready.Close()
	defer release.Close()
	defer target.Close()
	unix.CloseOnExec(3)
	unix.CloseOnExec(4)
	unix.CloseOnExec(5)
	encoded, err := io.ReadAll(io.LimitReader(metadata, maximumRequestBytes+1))
	if err != nil || len(encoded) == 0 || len(encoded) > maximumRequestBytes {
		return errors.New("exec-gate command metadata is empty or invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var command commandRequest
	if err := decoder.Decode(&command); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("exec-gate command metadata contains trailing JSON")
	}
	canonical, err := json.Marshal(command)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return errors.New("exec-gate command metadata is not canonical JSON")
	}
	if err := validateCommand(command); err != nil {
		return err
	}
	if err := authenticateHeldExecutable(
		target,
		command.ExecutableByteLength,
		command.ExecutableSHA256,
	); err != nil {
		return fmt.Errorf("authenticate exec-gate target: %w", err)
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		return err
	}
	releaseByte := make([]byte, 1)
	defer func() { releaseByte[0] = 0 }()
	if _, err := io.ReadFull(release, releaseByte); err != nil || releaseByte[0] != 1 {
		return errors.New("exec gate was not released by its owner")
	}
	extra := make([]byte, 1)
	count, readErr := release.Read(extra)
	extra[0] = 0
	if count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return errors.New("exec-gate release framing is invalid")
	}
	if err := os.Chdir(command.CWD); err != nil {
		return err
	}
	arguments := append([]string{command.Executable}, command.Arguments...)
	// The pathname remains argv[0] for diagnostics, while the kernel executes
	// the authenticated descriptor inherited from the sole owner.
	unix.CloseOnExec(targetExecutableDescriptor)
	return syscall.Exec(
		fmt.Sprintf("/proc/self/fd/%d", targetExecutableDescriptor),
		arguments,
		canonicalEnvironment(command.Environment),
	)
}

func readExecGateReady(reader io.Reader) error {
	ready := make([]byte, 1)
	defer func() { ready[0] = 0 }()
	if _, err := io.ReadFull(reader, ready); err != nil || ready[0] != 1 {
		return errors.New("exec gate did not emit its readiness byte")
	}
	return nil
}

func holdExecutable(
	path string,
	expectedByteLength *int64,
	expectedSHA256 *string,
) (*executableAuthority, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("owned executable is not an executable regular no-follow file")
	}
	if before.Size() < 1 || before.Size() > maximumExecutableBytes {
		return nil, errors.New("owned executable exceeds its bounded byte length")
	}
	if expectedByteLength != nil && before.Size() != *expectedByteLength {
		return nil, errors.New("owned executable byte length differs from its manifest")
	}
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New("owned executable descriptor could not be adopted")
	}
	closeOnFailure := true
	defer func() {
		if closeOnFailure {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil || !sameExecutableRevision(before, opened) {
		return nil, errors.Join(err, errors.New("owned executable changed while opened"))
	}
	digest, err := digestHeldExecutable(file, before.Size())
	if err != nil {
		return nil, err
	}
	if expectedSHA256 != nil && digest != *expectedSHA256 {
		return nil, errors.New("owned executable differs from its manifest digest")
	}
	authority := &executableAuthority{
		file: file, path: path, identity: before, byteLength: before.Size(), sha256: digest,
	}
	if err := authority.assertLive(); err != nil {
		return nil, err
	}
	closeOnFailure = false
	return authority, nil
}

func (authority *executableAuthority) assertLive() error {
	named, err := os.Lstat(authority.path)
	if err != nil {
		return err
	}
	opened, err := authority.file.Stat()
	if err != nil {
		return err
	}
	if !sameExecutableRevision(authority.identity, named) ||
		!sameExecutableRevision(authority.identity, opened) {
		return errors.New("owned executable changed while held")
	}
	digest, err := digestHeldExecutable(authority.file, authority.byteLength)
	if err != nil {
		return err
	}
	if digest != authority.sha256 {
		return errors.New("owned executable digest changed while held")
	}
	return nil
}

func (authority *executableAuthority) close() {
	if authority != nil && authority.file != nil {
		_ = authority.file.Close()
		authority.file = nil
	}
}

func closeLateExecutable(preflight <-chan executablePreflight) {
	go func() {
		result := <-preflight
		result.authority.close()
	}()
}

func authenticateHeldExecutable(
	file *os.File,
	expectedByteLength *int64,
	expectedSHA256 *string,
) error {
	metadata, err := file.Stat()
	if err != nil {
		return err
	}
	if !metadata.Mode().IsRegular() || metadata.Size() < 1 || metadata.Size() > maximumExecutableBytes ||
		metadata.Mode().Perm()&0o111 == 0 {
		return errors.New("exec-gate target is not a bounded executable regular file")
	}
	if expectedByteLength != nil && metadata.Size() != *expectedByteLength {
		return errors.New("exec-gate target byte length differs from its authority")
	}
	digest, err := digestHeldExecutable(file, metadata.Size())
	if err != nil {
		return err
	}
	if expectedSHA256 != nil && digest != *expectedSHA256 {
		return errors.New("exec-gate target differs from its authority digest")
	}
	return nil
}

func digestHeldExecutable(file *os.File, byteLength int64) (string, error) {
	digest := sha256.New()
	written, err := io.Copy(digest, io.NewSectionReader(file, 0, byteLength))
	if err != nil || written != byteLength {
		return "", errors.Join(err, errors.New("owned executable could not be read exactly"))
	}
	extra := make([]byte, 1)
	count, readErr := file.ReadAt(extra, byteLength)
	extra[0] = 0
	if count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return "", errors.New("owned executable grew beyond its held byte length")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func sameExecutableRevision(left, right os.FileInfo) bool {
	return os.SameFile(left, right) && left.Size() == right.Size() &&
		left.Mode() == right.Mode() && left.ModTime().Equal(right.ModTime())
}
