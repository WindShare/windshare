//go:build linux

package osfs

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	linuxNativeTestTempPattern        = ".windshare-osfs-test-*"
	linuxNativeFixtureEnvironment     = "WINDSHARE_LINUX_NATIVE_FIXTURE"
	linuxNativeFixtureVersion         = "loop-ext4-v1"
	linuxNativeFixtureTempEnvironment = "WINDSHARE_LINUX_NATIVE_TEMP_ROOT"
	linuxNativeTestDirectoryMode      = 0o700
)

func TestMain(m *testing.M) {
	testTemp, managed, err := linuxNativeTestTempRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "prepare Linux native-test temp root: %v\n", err)
		os.Exit(1)
	}

	previous, hadPrevious := os.LookupEnv("TMPDIR")
	if err := os.Setenv("TMPDIR", testTemp); err != nil {
		fmt.Fprintf(os.Stderr, "set Linux native-test TMPDIR: %v\n", err)
		if managed {
			_ = os.RemoveAll(testTemp)
		}
		os.Exit(1)
	}
	code := m.Run()
	if hadPrevious {
		_ = os.Setenv("TMPDIR", previous)
	} else {
		_ = os.Unsetenv("TMPDIR")
	}
	if managed {
		if err := os.RemoveAll(testTemp); err != nil && code == 0 {
			fmt.Fprintf(os.Stderr, "remove Linux native-test temp root: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

func linuxNativeTestTempRoot() (string, bool, error) {
	profile := os.Getenv(nativeOutputCertificationProfileEnvironment)
	fixture := os.Getenv(linuxNativeFixtureEnvironment)
	if profile == linuxExt4NativeCertificationProfile || fixture != "" {
		if profile != linuxExt4NativeCertificationProfile || fixture != linuxNativeFixtureVersion {
			return "", false, fmt.Errorf("required profile and native fixture marker disagree")
		}
		candidate := os.Getenv(linuxNativeFixtureTempEnvironment)
		if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
			return "", false, fmt.Errorf("native fixture temp root is not clean and absolute: %q", candidate)
		}
		info, err := os.Lstat(candidate)
		if err != nil {
			return "", false, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm() != os.FileMode(linuxNativeTestDirectoryMode) ||
			stat.Uid != uint32(os.Geteuid()) {
			return "", false, fmt.Errorf("native fixture temp root is not a receiver-owned 0700 directory")
		}
		return candidate, false, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve Linux native-test home: %w", err)
	}
	testTemp, err := os.MkdirTemp(home, linuxNativeTestTempPattern)
	if err != nil {
		return "", false, fmt.Errorf("create Linux native-test temp root: %w", err)
	}
	if err := os.Chmod(testTemp, linuxNativeTestDirectoryMode); err != nil {
		_ = os.RemoveAll(testTemp)
		return "", false, fmt.Errorf("make Linux native-test temp root private: %w", err)
	}
	return testTemp, true, nil
}
