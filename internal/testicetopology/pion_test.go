package testicetopology

import (
	"net"
	"path/filepath"
	"testing"

	pion "github.com/pion/webrtc/v4"
)

func TestPionPolicyUsesResolvedSourceAndEligibleRemoteInventory(t *testing.T) {
	profile := loadPionTestProfile(t)
	profileSHA256, err := profile.SHA256()
	if err != nil {
		t.Fatalf("profile hash: %v", err)
	}
	resolution, err := LoadResolution(
		filepath.Join("..", "..", "testdata", "test-ice-topology", "pr-same-host-kernel-route-ipv4-resolution.json"),
		profile,
		profileSHA256,
	)
	if err != nil {
		t.Fatalf("load resolution: %v", err)
	}
	policy, err := newPionNetworkPolicy(resolution)
	if err != nil {
		t.Fatalf("network policy: %v", err)
	}
	if !policy.allowsInterface("test-uplink0") || policy.allowsInterface("other") {
		t.Fatal("interface filter differs from the frozen resolution")
	}
	if !policy.allowsLocalIP(net.ParseIP("192.0.2.10")) || policy.allowsLocalIP(net.ParseIP("192.0.2.11")) {
		t.Fatal("local filter did not retain only the route-selected source")
	}
	if !policy.allowsRemoteIP(net.ParseIP("192.0.2.10")) || policy.allowsRemoteIP(net.ParseIP("2001:db8::1")) {
		t.Fatal("remote filter differs from the eligible IPv4 inventory")
	}
	if _, err := PionSettingEngine(profile, resolution); err != nil {
		t.Fatalf("setting engine: %v", err)
	}
}

func TestPionConfigurationNeverInheritsProductionSTUN(t *testing.T) {
	configuration, err := PionConfiguration(loadPionTestProfile(t))
	if err != nil {
		t.Fatalf("configuration: %v", err)
	}
	if configuration.ICEServers == nil || len(configuration.ICEServers) != 0 {
		t.Fatalf("ICE servers = %#v, want explicit empty registry", configuration.ICEServers)
	}
	if configuration.ICETransportPolicy != pion.ICETransportPolicyAll {
		t.Fatalf("ICE transport policy = %v", configuration.ICETransportPolicy)
	}
	invalid := loadPionTestProfile(t)
	invalid.TopologyID = "unknown"
	if _, err := PionConfiguration(invalid); err == nil {
		t.Fatal("unknown topology was accepted")
	}
}

func TestPionSettingEngineRejectsResolutionFromAnotherProfile(t *testing.T) {
	profile := loadPionTestProfile(t)
	resolution := Resolution{
		TopologyResolutionSchemaVersion: ResolutionSchemaVersion,
		TopologyID:                      profile.TopologyID,
		TopologyProfileSHA256:           "0000000000000000000000000000000000000000000000000000000000000000",
	}
	if _, err := PionSettingEngine(profile, resolution); err == nil {
		t.Fatal("unbound topology resolution was accepted")
	}
}

func loadPionTestProfile(t *testing.T) Profile {
	t.Helper()
	profile, err := Load(filepath.Join(
		"..", "..", "testdata", "test-ice-topology", "pr-same-host-kernel-route-ipv4.json",
	))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	return profile
}
