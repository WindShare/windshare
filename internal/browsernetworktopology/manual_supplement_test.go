package browsernetworktopology

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func TestManualRealNATSupplementHasAnIndependentCanonicalContract(t *testing.T) {
	encoded := mustReadFile(t, filepath.Join(
		"..", "..", "testdata", "browser-network-matrix", "supplemental",
		"manual-real-nat.profile.v1.json",
	))
	profile, err := ParseManualSupplementProfile(encoded)
	if err != nil {
		t.Fatalf("ParseManualSupplementProfile: %v", err)
	}
	canonical, err := profile.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, encoded) {
		t.Fatalf("manual supplement canonical bytes differ: err=%v", err)
	}
	if profile.ProfileID != ManualRealNATProfileID ||
		profile.ReportingSemantics != ManualSupplementReportingSemantics {
		t.Fatalf("manual supplement boundary differs: %+v", profile)
	}

	contract, _, _ := loadFixtureContract(t)
	if _, _, found := contract.Profile(profile.ProfileID); found {
		t.Fatal("manual supplement entered the scheduled hard profile registry")
	}
	if _, err := contract.ExpectedIdentities(ExecutionMode("manual")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("manual supplement entered the scheduled hard identity universe: %v", err)
	}
}

func TestManualRealNATSupplementRejectsHardProfileAndUnknownFields(t *testing.T) {
	encoded := mustReadFile(t, filepath.Join(
		"..", "..", "testdata", "browser-network-matrix", "supplemental",
		"manual-real-nat.profile.v1.json",
	))
	for name, mutated := range map[string][]byte{
		"hard schema": bytes.Replace(
			encoded,
			[]byte(ManualSupplementProfileSchemaVersion),
			[]byte(ProfileSchemaVersion),
			1,
		),
		"unknown field": addRootMember(encoded, `"executionMode":"manual"`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManualSupplementProfile(mutated); !errors.Is(err, ErrInvalidManualSupplement) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidManualSupplement)
			}
		})
	}
}
