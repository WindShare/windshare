//go:build linux

package outputlinux

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	linuxNativeTestTempPattern                  = ".windshare-outputlinux-test-*"
	nativeOutputCertificationProfileEnvironment = "WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION"
	linuxExt4NativeCertificationProfile         = "linux-ext4"
	linuxNativeFixtureEnvironment               = "WINDSHARE_LINUX_NATIVE_FIXTURE"
	linuxNativeFixtureVersion                   = "loop-ext4-v1"
	linuxNativeFixtureTempEnvironment           = "WINDSHARE_LINUX_NATIVE_TEMP_ROOT"
)

func TestMain(testingMain *testing.M) {
	testTemp, managed, err := linuxNativeTestTempRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "prepare Linux backend-test temp root: %v\n", err)
		os.Exit(1)
	}

	previous, hadPrevious := os.LookupEnv("TMPDIR")
	if err := os.Setenv("TMPDIR", testTemp); err != nil {
		fmt.Fprintf(os.Stderr, "set Linux backend-test TMPDIR: %v\n", err)
		if managed {
			_ = os.RemoveAll(testTemp)
		}
		os.Exit(1)
	}
	code := testingMain.Run()
	if hadPrevious {
		_ = os.Setenv("TMPDIR", previous)
	} else {
		_ = os.Unsetenv("TMPDIR")
	}
	if managed {
		if err := os.RemoveAll(testTemp); err != nil && code == 0 {
			fmt.Fprintf(os.Stderr, "remove Linux backend-test temp root: %v\n", err)
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
			info.Mode().Perm() != os.FileMode(linuxOutputDirectoryMode) ||
			stat.Uid != uint32(os.Geteuid()) {
			return "", false, fmt.Errorf("native fixture temp root is not a receiver-owned 0700 directory")
		}
		return candidate, false, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve Linux backend-test home: %w", err)
	}
	testTemp, err := os.MkdirTemp(home, linuxNativeTestTempPattern)
	if err != nil {
		return "", false, fmt.Errorf("create Linux backend-test temp root: %w", err)
	}
	if err := os.Chmod(testTemp, linuxOutputDirectoryMode); err != nil {
		_ = os.RemoveAll(testTemp)
		return "", false, fmt.Errorf("make Linux backend-test temp root private: %w", err)
	}
	return testTemp, true, nil
}

func requireUnprivilegedLinuxExt4Certification(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		return
	}
	if os.Getenv(nativeOutputCertificationProfileEnvironment) == linuxExt4NativeCertificationProfile {
		t.Fatal("required Linux/ext4 certification must run as an ordinary unprivileged receiver")
	}
	t.Skip("Linux/ext4 native certification is meaningful only as an unprivileged receiver")
}
