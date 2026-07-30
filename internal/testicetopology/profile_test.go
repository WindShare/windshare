package testicetopology

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sharedProfileSHA256    = "7d1082df592602db632e83b538b6d758fb71c66971b46e685f992b2c7a76c7ae"
	sharedResolutionSHA256 = "2fac2dd7a746ea4a853081553db529d5a996c75fc81be53d54d843fc5cd64cf6"
)

func TestSharedProfileAndResolutionRoundTrip(t *testing.T) {
	t.Parallel()
	profile := loadSharedProfile(t)
	if profile.TopologyID != PRTopologyID || profile.AddressFamily != AddressFamilyV4 {
		t.Fatalf("profile = %+v", profile)
	}
	canonicalProfile, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON profile: %v", err)
	}
	if _, err := Parse(canonicalProfile); err != nil {
		t.Fatalf("Parse canonical profile: %v", err)
	}
	profileDigest, err := profile.SHA256()
	if err != nil {
		t.Fatalf("SHA256 profile: %v", err)
	}
	if profileDigest != sharedProfileSHA256 {
		t.Fatalf("profile digest = %s, want %s", profileDigest, sharedProfileSHA256)
	}

	resolutionPath := sharedFixturePath("pr-same-host-kernel-route-ipv4-resolution.json")
	resolution, err := LoadResolution(resolutionPath, profile, sharedProfileSHA256)
	if err != nil {
		t.Fatalf("LoadResolution: %v", err)
	}
	if resolution.Interface.Index != 7 || resolution.Interface.SelectedAddress != "192.0.2.10" {
		t.Fatalf("resolution = %+v", resolution)
	}
	canonicalResolution, err := resolution.CanonicalJSON(profile, sharedProfileSHA256)
	if err != nil {
		t.Fatalf("CanonicalJSON resolution: %v", err)
	}
	if _, err := ParseResolution(canonicalResolution, profile, sharedProfileSHA256); err != nil {
		t.Fatalf("ParseResolution canonical: %v", err)
	}
	resolutionDigest, err := resolution.SHA256(profile, sharedProfileSHA256)
	if err != nil {
		t.Fatalf("SHA256 resolution: %v", err)
	}
	if resolutionDigest != sharedResolutionSHA256 {
		t.Fatalf("resolution digest = %s, want %s", resolutionDigest, sharedResolutionSHA256)
	}
}

func TestStrictJSONSharedMutationCorpus(t *testing.T) {
	t.Parallel()
	profile := loadSharedProfile(t)
	canonical, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	encodedCorpus, err := os.ReadFile(sharedFixturePath("strict-json-invalid-cases.json"))
	if err != nil {
		t.Fatalf("ReadFile corpus: %v", err)
	}
	var corpus struct {
		TopologyContractCaseRegistrySchemaVersion int `json:"topologyContractCaseRegistrySchemaVersion"`
		Cases                                     []struct {
			Name        string `json:"name"`
			Needle      string `json:"needle"`
			Replacement string `json:"replacement"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(encodedCorpus, &corpus); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if corpus.TopologyContractCaseRegistrySchemaVersion != 1 || len(corpus.Cases) == 0 {
		t.Fatalf("unexpected corpus registry: %+v", corpus)
	}
	for _, testCase := range corpus.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			if count := strings.Count(string(canonical), testCase.Needle); count != 1 {
				t.Fatalf("needle count = %d, want 1", count)
			}
			mutated := strings.Replace(string(canonical), testCase.Needle, testCase.Replacement, 1)
			if _, err := Parse([]byte(mutated)); !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("Parse error = %v, want ErrInvalidProfile", err)
			}
		})
	}

	invalidUTF8 := append([]byte(nil), canonical...)
	invalidUTF8[len(invalidUTF8)-2] = 0xff
	if _, err := Parse(invalidUTF8); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("invalid UTF-8 error = %v, want ErrInvalidProfile", err)
	}
}

func TestParseRejectsNonExactProfiles(t *testing.T) {
	t.Parallel()
	profile := loadSharedProfile(t)
	canonical, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	valid := string(canonical)
	tests := map[string]string{
		"unknown root field":      strings.Replace(valid, `"topologyProfileSchemaVersion":1`, `"topologyProfileSchemaVersion":1,"machineAddress":"192.0.2.2"`, 1),
		"wrong-case root field":   strings.Replace(valid, `"topologyId"`, `"TopologyId"`, 1),
		"unknown nested field":    strings.Replace(valid, `"algorithm":"udp-connect-source-consensus-v1"`, `"algorithm":"udp-connect-source-consensus-v1","fallback":true`, 1),
		"missing profile version": strings.Replace(valid, `"topologyProfileSchemaVersion":1,`, "", 1),
		"wrong profile version":   strings.Replace(valid, `"topologyProfileSchemaVersion":1`, `"topologyProfileSchemaVersion":2`, 1),
		"wrong topology":          strings.Replace(valid, PRTopologyID, "unknown", 1),
		"wrong selector":          strings.Replace(valid, SourceSelectorAlgorithm, "udp-connect-source-consensus-v2", 1),
		"wrong family":            strings.Replace(valid, `"addressFamily":"ipv4"`, `"addressFamily":"ipv6"`, 1),
		"missing ICE servers":     strings.Replace(valid, `"iceServers":[],`, "", 1),
		"null ICE servers":        strings.Replace(valid, `"iceServers":[]`, `"iceServers":null`, 1),
		"STUN server":             strings.Replace(valid, `"iceServers":[]`, `"iceServers":[{}]`, 1),
		"wrong transport":         strings.Replace(valid, `"iceTransportPolicy":"all"`, `"iceTransportPolicy":"relay"`, 1),
		"reordered probes": strings.Replace(valid,
			`{"address":"192.0.2.1","port":9},{"address":"198.51.100.1","port":9}`,
			`{"address":"198.51.100.1","port":9},{"address":"192.0.2.1","port":9}`, 1),
		"missing probe":       strings.Replace(valid, `,{"address":"203.0.113.1","port":9}`, "", 1),
		"broadened pairs":     strings.Replace(valid, `["host","prflx"]`, `["host","prflx","relay"]`, 1),
		"reordered pairs":     strings.Replace(valid, `["host","prflx"]`, `["prflx","host"]`, 1),
		"broadened protocols": strings.Replace(valid, `["udp"]`, `["udp","tcp"]`, 1),
		"trailing JSON":       valid + `{}`,
		"non-JSON trailing":   valid + `x`,
		"empty":               "",
	}
	for name, encoded := range tests {
		name, encoded := name, encoded
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(encoded)); !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("Parse error = %v, want ErrInvalidProfile", err)
			}
		})
	}
}

func TestResolutionValidationRejectsContractDrift(t *testing.T) {
	t.Parallel()
	profile := loadSharedProfile(t)
	valid := loadSharedResolution(t, profile)
	tests := map[string]func(*Resolution){
		"wrong version":                           func(value *Resolution) { value.TopologyResolutionSchemaVersion = 2 },
		"wrong topology":                          func(value *Resolution) { value.TopologyID = "unknown" },
		"wrong profile digest":                    func(value *Resolution) { value.TopologyProfileSHA256 = strings.Repeat("f", 64) },
		"uppercase profile digest":                func(value *Resolution) { value.TopologyProfileSHA256 = strings.ToUpper(sharedProfileSHA256) },
		"wrong selector":                          func(value *Resolution) { value.SelectorAlgorithm = "udp-connect-source-consensus-v2" },
		"wrong family":                            func(value *Resolution) { value.AddressFamily = "ipv6" },
		"missing probe":                           func(value *Resolution) { value.ProbeResults = value.ProbeResults[:2] },
		"wrong probe destination":                 func(value *Resolution) { value.ProbeResults[0].DestinationAddress = "192.0.2.2" },
		"wrong probe port":                        func(value *Resolution) { value.ProbeResults[0].DestinationPort = 10 },
		"probe disagreement":                      func(value *Resolution) { value.ProbeResults[1].SourceAddress = "192.0.2.11" },
		"non-operational probe":                   func(value *Resolution) { value.ProbeResults[1].SourceAddress = "127.0.0.1" },
		"zero interface index":                    func(value *Resolution) { value.Interface.Index = 0 },
		"empty interface name":                    func(value *Resolution) { value.Interface.Name = "" },
		"replacement character in interface name": func(value *Resolution) { value.Interface.Name = "route\ufffdname" },
		"non-NFC interface name":                  func(value *Resolution) { value.Interface.Name = "e\u0301" },
		"overlong interface name":                 func(value *Resolution) { value.Interface.Name = strings.Repeat("x", 256) },
		"invalid selected address":                func(value *Resolution) { value.Interface.SelectedAddress = "240.0.0.1" },
		"empty inventory":                         func(value *Resolution) { value.Interface.EligibleAddresses = []EligibleAddress{} },
		"selected absent":                         func(value *Resolution) { value.Interface.EligibleAddresses[0].Address = "192.0.2.11" },
		"duplicate address": func(value *Resolution) {
			value.Interface.EligibleAddresses = append(value.Interface.EligibleAddresses,
				EligibleAddress{Address: "192.0.2.10", PrefixLength: 32})
		},
		"non-canonical address order": func(value *Resolution) {
			value.Interface.EligibleAddresses = []EligibleAddress{
				{Address: "192.0.2.11", PrefixLength: 24},
				{Address: "192.0.2.10", PrefixLength: 24},
			}
		},
		"zero prefix": func(value *Resolution) { value.Interface.EligibleAddresses[0].PrefixLength = 0 },
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := cloneResolution(valid)
			mutate(&candidate)
			if err := candidate.Validate(profile, sharedProfileSHA256); !errors.Is(err, ErrInvalidResolution) {
				t.Fatalf("Validate error = %v, want ErrInvalidResolution", err)
			}
		})
	}

	if err := valid.Validate(profile, strings.Repeat("F", 64)); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("invalid expected digest error = %v, want ErrInvalidResolution", err)
	}
	wrongCanonicalDigest := strings.Repeat("f", 64)
	wronglyBound := cloneResolution(valid)
	wronglyBound.TopologyProfileSHA256 = wrongCanonicalDigest
	if err := wronglyBound.Validate(profile, wrongCanonicalDigest); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("non-profile digest binding error = %v, want ErrInvalidResolution", err)
	}
}

func TestResolutionParseRejectsUnknownAndNonCanonicalJSON(t *testing.T) {
	t.Parallel()
	profile := loadSharedProfile(t)
	resolution := loadSharedResolution(t, profile)
	canonical, err := resolution.CanonicalJSON(profile, sharedProfileSHA256)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	valid := string(canonical)
	tests := map[string]string{
		"unknown root":      strings.Replace(valid, `"topologyResolutionSchemaVersion":1`, `"topologyResolutionSchemaVersion":1,"extra":true`, 1),
		"unknown probe":     strings.Replace(valid, `"destinationPort":9`, `"destinationPort":9,"extra":true`, 1),
		"unknown interface": strings.Replace(valid, `"index":7`, `"index":7,"extra":true`, 1),
		"unknown address":   strings.Replace(valid, `"prefixLength":24`, `"prefixLength":24,"extra":true`, 1),
		"duplicate nested":  strings.Replace(valid, `"index":7`, `"index":7,"index":7`, 1),
		"exponent integer":  strings.Replace(valid, `"index":7`, `"index":7e0`, 1),
		"wrong case":        strings.Replace(valid, `"selectedAddress"`, `"SelectedAddress"`, 1),
		"trailing":          valid + `{}`,
	}
	for name, encoded := range tests {
		name, encoded := name, encoded
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseResolution([]byte(encoded), profile, sharedProfileSHA256); !errors.Is(err, ErrInvalidResolution) {
				t.Fatalf("ParseResolution error = %v, want ErrInvalidResolution", err)
			}
		})
	}
	wrongCanonicalDigest := strings.Repeat("f", 64)
	wronglyBound := strings.Replace(valid, sharedProfileSHA256, wrongCanonicalDigest, 1)
	if _, err := ParseResolution([]byte(wronglyBound), profile, wrongCanonicalDigest); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("ParseResolution non-profile digest error = %v, want ErrInvalidResolution", err)
	}
}

func TestCanonicalResolutionUsesJSONStringifyEscaping(t *testing.T) {
	t.Parallel()
	profile := loadSharedProfile(t)
	resolution := loadSharedResolution(t, profile)
	resolution.Interface.Name = "route<&>\u2028\u2029"
	encoded, err := resolution.CanonicalJSON(profile, sharedProfileSHA256)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !json.Valid(encoded) {
		t.Fatalf("canonical resolution is invalid JSON: %s", encoded)
	}
	text := string(encoded)
	if !strings.Contains(text, "route<&>\u2028\u2029") || strings.Contains(text, `\u2028`) || strings.Contains(text, `\u2029`) {
		t.Fatalf("canonical string does not match JSON.stringify escaping: %s", text)
	}
}

func TestOperationalIPv4Boundary(t *testing.T) {
	t.Parallel()
	for _, rejected := range []string{
		"", "0.1.2.3", "127.0.0.1", "169.254.1.1", "224.0.0.1", "240.0.0.1",
		"255.255.255.255", "192.0.2.01", "192.0.2", "192.0.2.1.1", "192.0.2.256",
		"2001:db8::1", "-1.0.0.1",
	} {
		if IsOperationalIPv4Unicast(rejected) {
			t.Errorf("IsOperationalIPv4Unicast(%q) = true", rejected)
		}
	}
	for _, accepted := range []string{"10.0.0.1", "192.0.2.10", "223.255.255.254"} {
		if !IsOperationalIPv4Unicast(accepted) {
			t.Errorf("IsOperationalIPv4Unicast(%q) = false", accepted)
		}
	}
}

func TestLoadBoundsOpenErrorsAndInvalidMethods(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Load missing profile succeeded")
	}
	large := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", MaximumFileBytes+1)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(large); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("Load oversized error = %v, want ErrInvalidProfile", err)
	}
	profile := Profile{}
	if err := profile.Validate(); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("Validate profile error = %v", err)
	}
	if _, err := profile.CanonicalJSON(); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("CanonicalJSON profile error = %v", err)
	}
	if _, err := profile.SHA256(); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("SHA256 profile error = %v", err)
	}
	resolution := Resolution{}
	if _, err := resolution.CanonicalJSON(profile, sharedProfileSHA256); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("CanonicalJSON resolution error = %v", err)
	}
	if _, err := resolution.SHA256(profile, sharedProfileSHA256); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("SHA256 resolution error = %v", err)
	}
}

func loadSharedProfile(t *testing.T) Profile {
	t.Helper()
	profile, err := Load(sharedFixturePath("pr-same-host-kernel-route-ipv4.json"))
	if err != nil {
		t.Fatalf("Load profile: %v", err)
	}
	return profile
}

func loadSharedResolution(t *testing.T, profile Profile) Resolution {
	t.Helper()
	resolution, err := LoadResolution(
		sharedFixturePath("pr-same-host-kernel-route-ipv4-resolution.json"),
		profile,
		sharedProfileSHA256,
	)
	if err != nil {
		t.Fatalf("LoadResolution: %v", err)
	}
	return resolution
}

func cloneResolution(value Resolution) Resolution {
	cloned := value
	cloned.ProbeResults = append([]ProbeResult(nil), value.ProbeResults...)
	cloned.Interface.EligibleAddresses = append([]EligibleAddress(nil), value.Interface.EligibleAddresses...)
	return cloned
}

func sharedFixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "test-ice-topology", name)
}
