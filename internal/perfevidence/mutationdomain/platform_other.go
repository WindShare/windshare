//go:build !linux && !windows

package mutationdomain

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

type platformOutputAuthority struct {
	path string
}

type platformPromotedInput struct{}

var errUnsupportedMutationDomain = errors.New("private mutation domains require Linux namespaces or Windows AppContainer")

func maybeRunPlatformBroker([]string, io.Reader, io.Writer, io.Writer) (bool, int) {
	return false, 0
}

func maybeRunPlatformTarget([]string, io.Writer) (bool, int) { return false, 0 }

func platformTargetInvocation(executable string, arguments []string) (string, []string) {
	return executable, arguments
}
func preparePlatformTarget(*exec.Cmd) (func() error, func() error, func() error, error) {
	noop := func() error { return nil }
	return noop, noop, noop, nil
}

func openPlatformSession(context.Context, initialization) (*session, error) {
	return nil, errUnsupportedMutationDomain
}

func preparePlatformHelper(initialization) (string, map[string]string, func() error, error) {
	return "", nil, nil, errUnsupportedMutationDomain
}

func helperTargetProcessAttributes() *syscall.SysProcAttr { return nil }
func settlePlatformTarget() error                         { return nil }
func openPlatformOutputAuthority(path string) (*platformOutputAuthority, error) {
	return &platformOutputAuthority{path: path}, nil
}
func (*platformOutputAuthority) close() error { return nil }
func platformOpenProtectedOutput(authority *platformOutputAuthority, leaf string) (*os.File, func() error, error) {
	file, err := os.Open(filepath.Join(authority.path, leaf))
	return file, func() error { return nil }, err
}
func platformPromoteProtectedOutput(
	*os.File,
	func() error,
	int64,
	string,
	os.FileMode,
	string,
) (*platformPromotedInput, error) {
	return nil, errUnsupportedMutationDomain
}
func (*platformPromotedInput) path() string { return "" }
func (*platformPromotedInput) close() error { return nil }
