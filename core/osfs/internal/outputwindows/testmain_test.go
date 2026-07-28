//go:build windows

package outputwindows

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsNativeTestTempPattern = ".windshare-osfs-test-*"

func windowsV3SetTestDirectoryDACL(
	path string,
	descriptor *windows.SECURITY_DESCRIPTOR,
	policy *windowsV3PrivatePolicy,
) error {
	if descriptor == nil || policy == nil {
		return errors.New("test directory DACL policy is absent")
	}
	existingOwner, err := windowsV3TestDirectoryOwner(path)
	if err != nil {
		return err
	}
	if !policy.ancestryExempts(existingOwner) {
		return fmt.Errorf("Windows native-test directory has untrusted owner %s", existingOwner.String())
	}
	dacl, daclDefaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || daclDefaulted {
		return errors.Join(errors.New("test directory DACL is absent, null, or defaulted"), err)
	}
	// The freshly created directory already belongs to an owner selected by the
	// caller's token. That ownership supplies WRITE_DAC, while an ordinary
	// receiver need not hold WRITE_OWNER. Treat the descriptor owner as inert
	// test data: hostile-DACL fixtures deliberately describe other owners, but
	// must never change the filesystem object's actual ownership.
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return err
	}
	installedOwner, err := windowsV3TestDirectoryOwner(path)
	if err != nil {
		return err
	}
	if !installedOwner.Equals(existingOwner) {
		return fmt.Errorf(
			"test directory owner changed while installing its DACL: before=%s after=%s",
			existingOwner.String(),
			installedOwner.String(),
		)
	}
	return nil
}

func windowsV3TestDirectoryOwner(path string) (*windows.SID, error) {
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return nil, err
	}
	owner, ownerDefaulted, err := descriptor.Owner()
	if err != nil || owner == nil || ownerDefaulted || !owner.IsValid() {
		return nil, errors.Join(errors.New("test directory existing owner is absent, invalid, or defaulted"), err)
	}
	// Copy the descriptor-backed SID so callers never depend on the lifetime of
	// an interior pointer after the temporary security descriptor is released.
	return windows.StringToSid(owner.String())
}

func windowsV3ProtectedTestDescriptor(
	policy *windowsV3PrivatePolicy,
	owner *windows.SID,
) (*windows.SECURITY_DESCRIPTOR, error) {
	if policy == nil || owner == nil || !owner.IsValid() {
		return nil, errors.New("Windows native-test ACL policy is unavailable")
	}
	if !policy.ancestryExempts(owner) {
		return nil, fmt.Errorf("Windows native-test directory has untrusted owner %s", owner.String())
	}
	principals, err := windowsV3TestDirectoryPrincipals(policy)
	if err != nil {
		return nil, err
	}
	entries := ""
	for _, principal := range principals {
		entries += fmt.Sprintf("(A;OICI;GA;;;%s)", principal.String())
	}
	return windows.SecurityDescriptorFromString("O:" + owner.String() + "D:P" + entries)
}

func windowsV3TestDirectoryPrincipals(policy *windowsV3PrivatePolicy) ([]*windows.SID, error) {
	if policy == nil {
		return nil, errors.New("Windows native-test ACL policy is unavailable")
	}
	candidates := []*windows.SID{policy.userSID, policy.systemSID, policy.administratorsSID}
	principals := make([]*windows.SID, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || !candidate.IsValid() {
			return nil, errors.New("Windows native-test ACL policy contains an invalid principal")
		}
		// A service account can itself be LocalSystem. Collapsing duplicate ACEs
		// keeps the installed authority set exact and therefore auditable.
		if !slices.ContainsFunc(principals, candidate.Equals) {
			principals = append(principals, candidate)
		}
	}
	return principals, nil
}

func windowsV3VerifyTestDirectoryDescriptor(
	descriptor *windows.SECURITY_DESCRIPTOR,
	expectedOwner *windows.SID,
	policy *windowsV3PrivatePolicy,
) error {
	if descriptor == nil || expectedOwner == nil || !expectedOwner.IsValid() ||
		policy == nil || !policy.ancestryExempts(expectedOwner) {
		return errors.New("Windows native-test descriptor expectation is unavailable")
	}
	owner, ownerDefaulted, err := descriptor.Owner()
	if err != nil || owner == nil || ownerDefaulted || !owner.IsValid() || !owner.Equals(expectedOwner) {
		return errors.Join(errors.New("Windows native-test directory owner changed or became invalid"), err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("Windows native-test DACL is not protected from inheritance")
	}
	dacl, daclDefaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || daclDefaulted {
		return errors.Join(errors.New("Windows native-test DACL is absent or defaulted"), err)
	}
	principals, err := windowsV3TestDirectoryPrincipals(policy)
	if err != nil {
		return err
	}
	expectedCount := len(principals) * 2
	if int(dacl.AceCount) != expectedCount {
		return fmt.Errorf("Windows native-test DACL contains %d entries; expected %d", dacl.AceCount, expectedCount)
	}

	// Windows maps one inheritable GENERIC_ALL directory ACE into an object ACE
	// with native file rights plus an inherit-only generic template. Requiring
	// both halves proves that neither current nor descendant authority broadened.
	inheritFlags := uint8(windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE | windows.INHERIT_ONLY_ACE)
	objectFound := make([]bool, len(principals))
	inheritFound := make([]bool, len(principals))
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace == nil {
			return errors.New("Windows native-test DACL contains a nil access entry")
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("Windows native-test DACL contains access entry type=%d; expected type=%d",
				ace.Header.AceType, windows.ACCESS_ALLOWED_ACE_TYPE)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		principalIndex := -1
		for candidateIndex, principal := range principals {
			if sid.Equals(principal) {
				principalIndex = candidateIndex
				break
			}
		}
		if principalIndex < 0 {
			return errors.New("Windows native-test DACL grants an unexpected principal")
		}
		switch {
		case ace.Header.AceFlags == 0 && ace.Mask == windowsV3FileAllAccess:
			if objectFound[principalIndex] {
				return errors.New("Windows native-test DACL duplicates current-object authority")
			}
			objectFound[principalIndex] = true
		case ace.Header.AceFlags == inheritFlags && ace.Mask == windows.GENERIC_ALL:
			if inheritFound[principalIndex] {
				return errors.New("Windows native-test DACL duplicates descendant authority")
			}
			inheritFound[principalIndex] = true
		default:
			return fmt.Errorf(
				"Windows native-test DACL access entry flags=%#x mask=%#x has unexpected authority",
				ace.Header.AceFlags, ace.Mask,
			)
		}
	}
	for principalIndex := range principals {
		if !objectFound[principalIndex] || !inheritFound[principalIndex] {
			return fmt.Errorf("Windows native-test DACL omits required principal %s", principals[principalIndex].String())
		}
	}
	return nil
}

func TestWindowsV3TestDirectoryDescriptorPreservesExistingOwner(t *testing.T) {
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	owner, err := windowsV3TestDirectoryOwner(path)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windowsV3ProtectedTestDescriptor(policy, owner)
	if err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	beforeOwner, _, err := before.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsV3SetTestDirectoryDACL(path, descriptor, policy); err != nil {
		t.Fatal(err)
	}
	installed, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	installedOwner, _, err := installed.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if beforeOwner == nil || installedOwner == nil || !beforeOwner.Equals(installedOwner) {
		t.Fatalf("test directory owner changed while installing its DACL: before=%v after=%v", beforeOwner, installedOwner)
	}
	if err := windowsV3VerifyTestDirectoryDescriptor(installed, owner, policy); err != nil {
		t.Fatalf("verify exact installed test directory descriptor: %v", err)
	}
	if err := windowsV3VerifyAncestryAuthorityDescriptor(installed, policy); err != nil {
		t.Fatalf("verify installed test directory descriptor: %v", err)
	}
}

func TestWindowsV3TestDirectoryDACLDoesNotApplyDescriptorOwner(t *testing.T) {
	policy, err := newWindowsV3PrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	owner, err := windowsV3TestDirectoryOwner(path)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windowsV3ProtectedTestDescriptor(policy, owner)
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		t.Fatal(err)
	}
	var foreignOwner *windows.SID
	for _, candidate := range []*windows.SID{policy.userSID, policy.systemSID, policy.administratorsSID} {
		if !candidate.Equals(owner) {
			foreignOwner = candidate
			break
		}
	}
	if foreignOwner == nil {
		t.Fatal("Windows test policy has no exempt owner distinct from the directory owner")
	}
	if err := absolute.SetOwner(foreignOwner, false); err != nil {
		t.Fatal(err)
	}

	if err := windowsV3SetTestDirectoryDACL(path, absolute, policy); err != nil {
		t.Fatal(err)
	}
	installed, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	installedOwner, _, err := installed.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if installedOwner == nil || !installedOwner.Equals(owner) {
		t.Fatalf("descriptor owner changed test directory ownership: got=%v want=%v", installedOwner, owner)
	}
	if err := windowsV3VerifyTestDirectoryDescriptor(installed, owner, policy); err != nil {
		t.Fatalf("verify DACL installed independently of descriptor owner: %v", err)
	}
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
	owner, err := windowsV3TestDirectoryOwner(testTemp)
	if err != nil {
		fail("read Windows native-test temp owner", err)
	}
	descriptor, err := windowsV3ProtectedTestDescriptor(policy, owner)
	if err != nil {
		fail("construct Windows native-test ACL", err)
	}
	if err := windowsV3SetTestDirectoryDACL(testTemp, descriptor, policy); err != nil {
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
	if err := windowsV3VerifyTestDirectoryDescriptor(installed, owner, policy); err != nil {
		fail("verify exact Windows native-test temp ACL", err)
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
