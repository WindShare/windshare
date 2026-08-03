//go:build linux

package linuxsubreaper

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"strconv"
	"syscall"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	maximumExecutableBytes     = 512 << 20
	targetExecutableDescriptor = 6
	execEventDescriptor        = 7
	execResultDescriptor       = 8
	targetDirectoryDescriptor  = 9
)

type executableAuthority struct {
	file     *os.File
	path     string
	identity os.FileInfo
}

func runExecChild() (resultErr error) {
	metadata := os.NewFile(3, "exec-gate-metadata")
	ready := os.NewFile(4, "exec-gate-ready")
	release := os.NewFile(5, "exec-gate-release")
	target := os.NewFile(targetExecutableDescriptor, "exec-gate-target")
	result := os.NewFile(execResultDescriptor, "exec-gate-result")
	workingDirectory := os.NewFile(targetDirectoryDescriptor, "exec-gate-working-directory")
	if metadata == nil || ready == nil || release == nil || target == nil || result == nil || workingDirectory == nil {
		return errors.New("exec gate requires metadata, ready, release, target, result, and working-directory descriptors")
	}
	// Validate the result capability first so later gate failures can still be
	// published without trusting a descriptor that merely occupies fd 8.
	if err := validatePipeDescriptor(execResultDescriptor, unix.O_WRONLY, "exec-gate result"); err != nil {
		_ = result.Close()
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, writeExecFailure(result, "TARGET_EXEC_FAILED", resultErr))
		}
		resultErr = errors.Join(resultErr, result.Close())
	}()
	defer metadata.Close()
	defer ready.Close()
	defer release.Close()
	defer target.Close()
	defer workingDirectory.Close()
	if err := validateExecChildDescriptors(); err != nil {
		return err
	}
	for _, capability := range []struct {
		descriptor int
		label      string
	}{
		{descriptor: 3, label: "exec-gate metadata"},
		{descriptor: 4, label: "exec-gate readiness"},
		{descriptor: 5, label: "exec-gate release"},
		{descriptor: targetExecutableDescriptor, label: "exec-gate target"},
		{descriptor: execResultDescriptor, label: "exec-gate result"},
		{descriptor: targetDirectoryDescriptor, label: "exec-gate working directory"},
	} {
		if err := setDescriptorInherited(capability.descriptor, false, capability.label); err != nil {
			return err
		}
	}
	metadataRecord, err := ownerprotocol.ReadDocument[execGateMetadata](metadata)
	if err != nil {
		return err
	}
	if err := ownerprotocol.ValidateCommand(metadataRecord.Command); err != nil {
		return err
	}
	if err := ownerprotocol.ValidateIdentity(metadataRecord.Identity); err != nil {
		return err
	}
	if err := prepareEventDescriptor(metadataRecord.EventDescriptor); err != nil {
		return err
	}
	if err := authenticateHeldExecutable(target); err != nil {
		return fmt.Errorf("authenticate exec-gate target: %w", err)
	}
	if err := authenticateHeldWorkingDirectory(workingDirectory); err != nil {
		return fmt.Errorf("authenticate exec-gate working directory: %w", err)
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
	command := metadataRecord.Command
	if err := unix.Fchdir(targetDirectoryDescriptor); err != nil {
		return err
	}
	arguments := append([]string{command.Executable}, command.Arguments...)
	if err := writeExecAttempt(result); err != nil {
		return fmt.Errorf("publish exec attempt: %w", err)
	}
	environment := canonicalEnvironment(command.Environment, metadataRecord.EventDescriptor, metadataRecord.Identity)
	if err := execveat(targetExecutableDescriptor, arguments, environment); !errors.Is(err, syscall.ENOENT) {
		return err
	}
	// Linux cannot execute an interpreter script through a close-on-exec O_PATH
	// descriptor. Retry only the documented ENOENT case with the capability
	// inherited; native binaries keep the descriptor fenced by CLOEXEC.
	if err := setDescriptorInherited(targetExecutableDescriptor, true, "exec-gate script target"); err != nil {
		return err
	}
	return execveat(targetExecutableDescriptor, arguments, environment)
}

func prepareEventDescriptor(descriptor int) error {
	if descriptor == 0 {
		if err := validateNullDescriptor(execEventDescriptor, "exec-gate event placeholder"); err != nil {
			return err
		}
		return setDescriptorInherited(execEventDescriptor, false, "exec-gate event placeholder")
	}
	if descriptor != execEventDescriptor {
		return errors.New("exec-gate test-event descriptor is invalid")
	}
	if err := validatePipeDescriptor(descriptor, unix.O_WRONLY, "exec-gate test event"); err != nil {
		return err
	}
	return setDescriptorInherited(descriptor, true, "exec-gate test event")
}

func readExecGateReady(reader io.Reader) error {
	ready := make([]byte, 1)
	defer func() { ready[0] = 0 }()
	if _, err := io.ReadFull(reader, ready); err != nil || ready[0] != 1 {
		return errors.New("exec gate did not emit its readiness byte")
	}
	return nil
}

func holdExecutable(path string) (*executableAuthority, error) {
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
	descriptor, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
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
	authority := &executableAuthority{file: file, path: path, identity: before}
	if err := authority.assertLive(); err != nil {
		return nil, err
	}
	closeOnFailure = false
	return authority, nil
}

func (authority *executableAuthority) startEvidence(
	identity ownerprotocol.Identity,
	root processIdentity,
) (ownerprotocol.StartEvidence, error) {
	if authority == nil || authority.file == nil || authority.identity == nil {
		return ownerprotocol.StartEvidence{}, errors.New("owned executable authority is unavailable")
	}
	metadata, ok := authority.identity.Sys().(*syscall.Stat_t)
	if !ok || metadata.Ino == 0 || root.PID < 1 || root.StartTimeTicks == 0 {
		return ownerprotocol.StartEvidence{}, errors.New("owned executable or process instance identity is unavailable")
	}
	return ownerprotocol.StartEvidence{
		SchemaVersion:   ownerprotocol.StartEvidenceSchemaVersion,
		Identity:        identity,
		Platform:        ownerprotocol.PlatformLinuxSubreaper,
		ProcessID:       root.PID,
		ProcessInstance: strconv.FormatUint(root.StartTimeTicks, 10),
		Executable:      ownerprotocol.NewObjectIdentity64(metadata.Dev, metadata.Ino),
	}, nil
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
	return nil
}

func (authority *executableAuthority) close() {
	if authority != nil && authority.file != nil {
		_ = authority.file.Close()
		authority.file = nil
	}
}

func authenticateHeldExecutable(file *os.File) error {
	if err := validatePathDescriptor(int(file.Fd()), "exec-gate target"); err != nil {
		return err
	}
	metadata, err := file.Stat()
	if err != nil {
		return err
	}
	if !metadata.Mode().IsRegular() || metadata.Size() < 1 || metadata.Size() > maximumExecutableBytes ||
		metadata.Mode().Perm()&0o111 == 0 {
		return errors.New("exec-gate target is not a bounded executable regular file")
	}
	return nil
}

func sameExecutableRevision(left, right os.FileInfo) bool {
	return os.SameFile(left, right) && left.Size() == right.Size() &&
		left.Mode() == right.Mode() && left.ModTime().Equal(right.ModTime())
}
