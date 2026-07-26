//go:build windows

package osfs

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

const windowsNativeTestTempPattern = ".windshare-osfs-test-*"

func windowsV3SetTestDirectoryDescriptor(
	path string,
	descriptor *windows.SECURITY_DESCRIPTOR,
) error {
	if descriptor == nil {
		return errors.New("test directory security descriptor is absent")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		nil,
		dacl,
		nil,
	)
}

func windowsV3ProtectedTestDescriptor(policy *windowsV3PrivatePolicy) (*windows.SECURITY_DESCRIPTOR, error) {
	if policy == nil || policy.userSID == nil || policy.systemSID == nil || policy.administratorsSID == nil {
		return nil, errors.New("Windows native-test ACL policy is unavailable")
	}
	entries := fmt.Sprintf("(A;OICI;GA;;;%s)", policy.userSID.String()) +
		fmt.Sprintf("(A;OICI;GA;;;%s)", policy.systemSID.String()) +
		fmt.Sprintf("(A;OICI;GA;;;%s)", policy.administratorsSID.String())
	return windows.SecurityDescriptorFromString("O:" + policy.userSID.String() + "D:P" + entries)
}

func TestMain(m *testing.M) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve Windows native-test home: %v\n", err)
		os.Exit(1)
	}
	testTemp, err := os.MkdirTemp(home, windowsNativeTestTempPattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create Windows native-test temp root: %v\n", err)
		os.Exit(1)
	}
	fail := func(message string, err error) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", message, err)
		_ = os.RemoveAll(testTemp)
		os.Exit(1)
	}
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		fail("prepare Windows native-test ACL policy", err)
	}
	descriptor, err := windowsV3ProtectedTestDescriptor(policy)
	if err != nil {
		fail("construct Windows native-test ACL", err)
	}
	if err := windowsV3SetTestDirectoryDescriptor(testTemp, descriptor); err != nil {
		fail("protect Windows native-test temp root", err)
	}
	installed, err := windows.GetNamedSecurityInfo(
		testTemp,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		fail("read Windows native-test temp ACL", err)
	}
	if err := windowsV3VerifyAncestryAuthorityDescriptor(installed, policy); err != nil {
		fail("verify Windows native-test temp ACL", err)
	}

	previousTemp, hadTemp := os.LookupEnv("TEMP")
	previousTMP, hadTMP := os.LookupEnv("TMP")
	if err := os.Setenv("TEMP", testTemp); err != nil {
		fail("set Windows native-test TEMP", err)
	}
	if err := os.Setenv("TMP", testTemp); err != nil {
		if hadTemp {
			_ = os.Setenv("TEMP", previousTemp)
		} else {
			_ = os.Unsetenv("TEMP")
		}
		fail("set Windows native-test TMP", err)
	}

	code := m.Run()
	if hadTemp {
		_ = os.Setenv("TEMP", previousTemp)
	} else {
		_ = os.Unsetenv("TEMP")
	}
	if hadTMP {
		_ = os.Setenv("TMP", previousTMP)
	} else {
		_ = os.Unsetenv("TMP")
	}
	if err := os.RemoveAll(testTemp); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove Windows native-test temp root: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
