//go:build windows

package mutationdomain

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsProfileLedgerRecoversAbandonedReservedProfile(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, err := openWindowsProfileLedger(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ledger.close(); err != nil {
			t.Error(err)
		}
	})
	profileName := testEphemeralAppContainerProfileName(t)
	marker, err := createAppContainerRecoveryMarker(runtimeRoot, profileName)
	if err != nil {
		t.Fatal(err)
	}
	packageSID, err := createEphemeralAppContainerProfile(profileName)
	if err != nil {
		t.Fatal(err)
	}
	expectedSID := packageSID.String()
	if err := releaseNativeAppContainerSID(packageSID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = deleteEphemeralAppContainerProfile(profileName) })

	if err := ledger.recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered profile marker still exists: %v", err)
	}

	recreatedSID, err := createEphemeralAppContainerProfile(profileName)
	if err != nil {
		t.Fatalf("recreate recovered profile: %v", err)
	}
	if recreatedSID.String() != expectedSID {
		t.Fatalf("recreated profile SID = %s, want deterministic %s", recreatedSID.String(), expectedSID)
	}
	if err := releaseNativeAppContainerSID(recreatedSID); err != nil {
		t.Error(err)
	}
	if err := deleteEphemeralAppContainerProfile(profileName); err != nil {
		t.Error(err)
	}
}

func TestWindowsProfileLedgerFailsClosedForUnknownEntry(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, err := openWindowsProfileLedger(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.close() }()
	unknown := filepath.Join(ledger.directory, "not-a-reserved-profile.pending")
	if err := os.WriteFile(unknown, []byte("untrusted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ledger.recover(); err == nil || !strings.Contains(err.Error(), "unrecognized entry") {
		t.Fatalf("recover unknown ledger entry = %v, want fail-closed rejection", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown entry was destructively removed: %v", err)
	}
}

func TestWindowsProfileLedgerPreservesMarkerWhenProfileDeletionFails(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, err := openWindowsProfileLedger(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.close() }()
	profileName := testEphemeralAppContainerProfileName(t)
	marker, err := createAppContainerRecoveryMarker(runtimeRoot, profileName)
	if err != nil {
		t.Fatal(err)
	}
	deletionFailure := errors.New("injected profile deletion failure")
	ledger.deleteProfile = func(string) error { return deletionFailure }
	if err := ledger.recover(); !errors.Is(err, deletionFailure) {
		t.Fatalf("recover with failed profile deletion = %v, want injected failure", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("profile marker was removed before deletion succeeded: %v", err)
	}
}

func TestEphemeralAppContainerProfileNameValidation(t *testing.T) {
	valid := appContainerProfilePrefix + strings.Repeat("a", appContainerProfileEntropyHexBytes)
	if !validEphemeralAppContainerProfileName(valid) {
		t.Fatalf("reserved profile name rejected: %s", valid)
	}
	for _, invalid := range []string{
		"",
		"WindShare.Performance.a",
		appContainerProfilePrefix + strings.Repeat("A", appContainerProfileEntropyHexBytes),
		appContainerProfilePrefix + strings.Repeat("g", appContainerProfileEntropyHexBytes),
		"Other.Performance." + strings.Repeat("a", appContainerProfileEntropyHexBytes),
	} {
		if validEphemeralAppContainerProfileName(invalid) {
			t.Fatalf("unreserved profile name accepted: %q", invalid)
		}
	}
}

func testEphemeralAppContainerProfileName(t *testing.T) string {
	t.Helper()
	entropy, err := randomBytes(appContainerProfileEntropyBytes)
	if err != nil {
		t.Fatal(err)
	}
	return appContainerProfilePrefix + hex.EncodeToString(entropy)
}
