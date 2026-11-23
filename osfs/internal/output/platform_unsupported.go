//go:build !windows && !linux && !darwin

package output

import (
	"io/fs"
	"os"
	"time"
)

func resolveContainerTarget(string, os.FileInfo, func(string) (os.FileInfo, error)) (string, os.FileInfo, error) {
	return "", nil, ErrUnsupportedOutput
}

func openDirHandle(string) (*Dir, error)                      { return nil, ErrUnsupportedOutput }
func openVerifiedDirHandle(string, os.FileInfo) (*Dir, error) { return nil, ErrUnsupportedOutput }
func openChildDir(*Dir, string) (*Dir, error)                 { return nil, ErrUnsupportedOutput }
func openChildFile(*Dir, string, int, fs.FileMode) (*os.File, error) {
	return nil, ErrUnsupportedOutput
}
func lstatChild(*Dir, string) (EntryKind, uint64, error)         { return 0, 0, ErrUnsupportedOutput }
func mkdirChild(*Dir, string, fs.FileMode) error                 { return ErrUnsupportedOutput }
func removeChild(*Dir, string, bool) error                       { return ErrUnsupportedOutput }
func readDirNames(*Dir) ([]string, error)                        { return nil, ErrUnsupportedOutput }
func validateNativeName(string) error                            { return ErrUnsupportedOutput }
func isNativeNameRefusal(error) bool                             { return false }
func entryMatchesSpelling(*Dir, string) (bool, error)            { return false, ErrUnsupportedOutput }
func syncDir(*os.File) error                                     { return ErrUnsupportedOutput }
func setFileModifiedTime(*os.File, time.Time) error              { return ErrUnsupportedOutput }
func setDirModifiedTime(*Dir, time.Time) error                   { return ErrUnsupportedOutput }
func acquireDirLock(*Dir) (*DirLock, error)                      { return nil, ErrUnsupportedOutput }
func lockFile(*os.File) (func() error, error)                    { return nil, ErrUnsupportedOutput }
func renameFileNoReplace(*Dir, string, *Dir, string, bool) error { return ErrUnsupportedOutput }
func classifyPlatformError(error) (NativeErrorClass, bool)       { return 0, false }
