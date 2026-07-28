//go:build windows

package osfs

import (
	"fmt"
	"os"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
)

const windowsNativeTestTempPattern = ".windshare-osfs-test-*"

func TestMain(m *testing.M) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve Windows native-test home: %v\n", err)
		os.Exit(1)
	}
	testTemp, err := os.MkdirTemp(home, windowsNativeTestTempPattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reserve Windows native-test temp root: %v\n", err)
		os.Exit(1)
	}
	if err := os.Remove(testTemp); err != nil {
		fmt.Fprintf(os.Stderr, "release Windows native-test temp reservation: %v\n", err)
		os.Exit(1)
	}
	platform, err := outputwindows.Open(testTemp, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create certified Windows native-test temp root: %v\n", err)
		os.Exit(1)
	}
	if err := platform.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close certified Windows native-test temp root: %v\n", err)
		_ = os.RemoveAll(testTemp)
		os.Exit(1)
	}

	previousTemp, hadTemp := os.LookupEnv("TEMP")
	previousTMP, hadTMP := os.LookupEnv("TMP")
	if err := os.Setenv("TEMP", testTemp); err != nil {
		fmt.Fprintf(os.Stderr, "set Windows native-test TEMP: %v\n", err)
		_ = os.RemoveAll(testTemp)
		os.Exit(1)
	}
	if err := os.Setenv("TMP", testTemp); err != nil {
		restoreWindowsTestEnvironment("TEMP", previousTemp, hadTemp)
		fmt.Fprintf(os.Stderr, "set Windows native-test TMP: %v\n", err)
		_ = os.RemoveAll(testTemp)
		os.Exit(1)
	}

	code := m.Run()
	restoreWindowsTestEnvironment("TEMP", previousTemp, hadTemp)
	restoreWindowsTestEnvironment("TMP", previousTMP, hadTMP)
	if err := os.RemoveAll(testTemp); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove Windows native-test temp root: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

func restoreWindowsTestEnvironment(name, value string, present bool) {
	if present {
		_ = os.Setenv(name, value)
		return
	}
	_ = os.Unsetenv(name)
}
