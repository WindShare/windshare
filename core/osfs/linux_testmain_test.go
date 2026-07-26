//go:build linux

package osfs

import (
	"fmt"
	"os"
	"testing"
)

const linuxNativeTestTempPattern = ".windshare-osfs-test-*"

func TestMain(m *testing.M) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve Linux native-test home: %v\n", err)
		os.Exit(1)
	}
	testTemp, err := os.MkdirTemp(home, linuxNativeTestTempPattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create Linux native-test temp root: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(testTemp, linuxOutputDirectoryMode); err != nil {
		fmt.Fprintf(os.Stderr, "make Linux native-test temp root private: %v\n", err)
		_ = os.RemoveAll(testTemp)
		os.Exit(1)
	}

	previous, hadPrevious := os.LookupEnv("TMPDIR")
	if err := os.Setenv("TMPDIR", testTemp); err != nil {
		fmt.Fprintf(os.Stderr, "set Linux native-test TMPDIR: %v\n", err)
		_ = os.RemoveAll(testTemp)
		os.Exit(1)
	}
	code := m.Run()
	if hadPrevious {
		_ = os.Setenv("TMPDIR", previous)
	} else {
		_ = os.Unsetenv("TMPDIR")
	}
	if err := os.RemoveAll(testTemp); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove Linux native-test temp root: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
