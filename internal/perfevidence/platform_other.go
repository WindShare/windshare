//go:build !windows && !linux

package perfevidence

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

var errUnsupportedPublicationPlatform = errors.New("performance evidence publication requires Windows or Linux filesystem authorities")

type outputRootAuthority struct {
	path string
}
type stageDirectoryAuthority struct {
	path       string
	name       string
	transition func(relative, phase string) error
}

func acquireConsumptionAuthority(
	[]snapshotValidationTarget,
	[]string,
) (byteConsumptionAuthority, error) {
	return nil, errUnsupportedPublicationPlatform
}

func prepareMutationOutput(string) (mutationOutputSink, error) {
	return nil, errUnsupportedPublicationPlatform
}

func openOutputRootAuthority(string) (*outputRootAuthority, error) {
	return nil, errUnsupportedPublicationPlatform
}
func openTreeAuthority(string) (*stageDirectoryAuthority, error) {
	return nil, errUnsupportedPublicationPlatform
}

func directoryIdentityAt(string) (directoryIdentity, error) {
	return directoryIdentity{}, errUnsupportedPublicationPlatform
}

func (*outputRootAuthority) verifyPath() error { return errUnsupportedPublicationPlatform }
func (*outputRootAuthority) close() error      { return nil }
func (*outputRootAuthority) createChildAuthority(string) (*stageDirectoryAuthority, error) {
	return nil, errUnsupportedPublicationPlatform
}
func (*outputRootAuthority) openChildAuthority(string) (*stageDirectoryAuthority, error) {
	return nil, errUnsupportedPublicationPlatform
}
func (*outputRootAuthority) openRecoveryChildAuthority(string) (*stageDirectoryAuthority, error) {
	return nil, errUnsupportedPublicationPlatform
}
func (*outputRootAuthority) readDir() ([]os.DirEntry, error) {
	return nil, errUnsupportedPublicationPlatform
}
func (*outputRootAuthority) removeChild(string, func(string) error) error {
	return errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) verifyName(*outputRootAuthority) error {
	return errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) close() error { return nil }
func (*stageDirectoryAuthority) acquireLiveLease(*outputRootAuthority) error {
	return errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) tryAcquireRecoveryLease(*outputRootAuthority) (bool, error) {
	return false, errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) modTime() (time.Time, error) {
	return time.Time{}, errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) matchesAuthority(*stageDirectoryAuthority) error {
	return errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) openRegularFile(string) (*os.File, os.FileInfo, error) {
	return nil, nil, errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) walkRegularFiles(regularFileVisitor) error {
	return errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) walkEvidenceStore(*evidenceStoreWalk) error {
	return errUnsupportedPublicationPlatform
}
func (*stageDirectoryAuthority) syncContents() error { return errUnsupportedPublicationPlatform }
func (*outputRootAuthority) removeRetainedChild(*stageDirectoryAuthority, func(string) error) error {
	return errUnsupportedPublicationPlatform
}
func authorityChildAbsent(error) bool { return false }

func platformPathKey(path string) string {
	return filepath.Clean(path)
}

func platformPathAlias(left, right string) bool {
	return false
}

func physicalMemory() (uint64, string, error) {
	return 0, "unsupported", errors.New("no native physical-memory probe for this OS")
}

func cpuModel() (string, error) {
	return os.Getenv("PROCESSOR_IDENTIFIER"), nil
}

func osDescription() string {
	return runtime.GOOS
}

func currentProcessToken() (string, error) {
	return "", errUnsupportedPublicationPlatform
}

func processMatches(processID int, token string) (bool, error) {
	return false, errUnsupportedPublicationPlatform
}

func (*outputRootAuthority) sync() error {
	return errUnsupportedPublicationPlatform
}
func (*outputRootAuthority) renameChildNoReplace(*stageDirectoryAuthority, string) error {
	return errUnsupportedPublicationPlatform
}
