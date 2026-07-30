package browsernetworktopology

import (
	"path"
	"regexp"
	"strings"
)

const (
	ManifestSchemaVersion           = "windshare.browser-network-matrix.manifest/v1"
	ProfileSchemaVersion            = "windshare.browser-network-matrix.profile/v1"
	RuntimeAttestationSchemaVersion = "windshare.browser-network-matrix.runtime-attestation/v2"
	SampleResultSchemaVersion       = "windshare.browser-network-matrix.sample-result/v1"
	RunResultSchemaVersion          = "windshare.browser-network-matrix.run-result/v1"
	AggregateSchemaVersion          = "windshare.browser-network-matrix.aggregate/v1"

	MatrixID                    = "phase3-observational-browser-network-v1"
	ObservationalNonblocking    = "observational-nonblocking"
	SamplesPerBrowser           = 5
	ScheduledIdentityCount      = 45
	ManualIdentityCount         = 15
	TotalIdentityCount          = 60
	identitiesPerProfile        = 15
	maximumIdentifierTextLength = 96
)

type Browser string

const (
	BrowserChromium Browser = "chromium"
	BrowserFirefox  Browser = "firefox"
	BrowserWebKit   Browser = "webkit"
)

type ExecutionMode string

const (
	ModeScheduled ExecutionMode = "scheduled"
	ModeManual    ExecutionMode = "manual"
)

type ProfileKind string

const (
	ProfileScheduledPublicSTUN    ProfileKind = "scheduled-public-stun"
	ProfileScheduledRestrictedUDP ProfileKind = "scheduled-restricted-udp"
	ProfileScheduledCoturn        ProfileKind = "scheduled-coturn"
	ProfileManualRealNAT          ProfileKind = "manual-real-nat"
)

type AuthorityKind string

const (
	AuthorityExternalFixture AuthorityKind = "external-fixture"
)

const AvailabilityNotAssumed = "not-assumed"

type ConnectivityExpectation string

const (
	ConnectivityEstablished ConnectivityExpectation = "connectivity-established"
	ConnectivityBlocked     ConnectivityExpectation = "connectivity-blocked"
)

type SelectedPairRequirement string

const (
	SelectedPairRequired   SelectedPairRequirement = "required"
	SelectedPairProhibited SelectedPairRequirement = "prohibited"
)

type CandidateType string

const (
	CandidateHost  CandidateType = "host"
	CandidateSRFLX CandidateType = "srflx"
	CandidatePRFLX CandidateType = "prflx"
	CandidateRelay CandidateType = "relay"
)

type TransportProtocol string

const (
	ProtocolUDP TransportProtocol = "udp"
	ProtocolTCP TransportProtocol = "tcp"
)

type profileSpec struct {
	profileID     string
	kind          ProfileKind
	executionMode ExecutionMode
	authorityID   string
	authorityKind AuthorityKind
	profilePath   string
	connectivity  ConnectivityExpectation
}

var frozenProfileSpecs = []profileSpec{
	{
		profileID:     string(ProfileScheduledPublicSTUN),
		kind:          ProfileScheduledPublicSTUN,
		executionMode: ModeScheduled,
		authorityID:   "public-stun-external-fixture",
		authorityKind: AuthorityExternalFixture,
		profilePath:   "profiles/scheduled-public-stun.v1.json",
		connectivity:  ConnectivityEstablished,
	},
	{
		profileID:     string(ProfileScheduledRestrictedUDP),
		kind:          ProfileScheduledRestrictedUDP,
		executionMode: ModeScheduled,
		authorityID:   "restricted-udp-external-fixture",
		authorityKind: AuthorityExternalFixture,
		profilePath:   "profiles/scheduled-restricted-udp.v1.json",
		connectivity:  ConnectivityBlocked,
	},
	{
		profileID:     string(ProfileScheduledCoturn),
		kind:          ProfileScheduledCoturn,
		executionMode: ModeScheduled,
		authorityID:   "coturn-external-fixture",
		authorityKind: AuthorityExternalFixture,
		profilePath:   "profiles/scheduled-coturn.v1.json",
		connectivity:  ConnectivityEstablished,
	},
	{
		profileID:     string(ProfileManualRealNAT),
		kind:          ProfileManualRealNAT,
		executionMode: ModeManual,
		authorityID:   "real-nat-external-fixture",
		authorityKind: AuthorityExternalFixture,
		profilePath:   "profiles/manual-real-nat.v1.json",
		connectivity:  ConnectivityEstablished,
	},
}

var (
	frozenBrowsers       = []Browser{BrowserChromium, BrowserFirefox, BrowserWebKit}
	frozenSampleOrdinals = []int{1, 2, 3, 4, 5}
	candidateTypeOrder   = []CandidateType{CandidateHost, CandidateSRFLX, CandidatePRFLX, CandidateRelay}
	protocolOrder        = []TransportProtocol{ProtocolUDP, ProtocolTCP}
	identifierPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
)

func findProfileSpec(profileID string) (profileSpec, bool) {
	for _, spec := range frozenProfileSpecs {
		if spec.profileID == profileID {
			return spec, true
		}
	}
	return profileSpec{}, false
}

func validIdentifier(value string) bool {
	return len(value) <= maximumIdentifierTextLength && identifierPattern.MatchString(value)
}

func validExecutionMode(mode ExecutionMode) bool {
	return mode == ModeScheduled || mode == ModeManual
}

func validBrowser(browser Browser) bool {
	return browser == BrowserChromium || browser == BrowserFirefox || browser == BrowserWebKit
}

func validCandidateType(candidateType CandidateType) bool {
	for _, expected := range candidateTypeOrder {
		if candidateType == expected {
			return true
		}
	}
	return false
}

func validProtocol(protocol TransportProtocol) bool {
	return protocol == ProtocolUDP || protocol == ProtocolTCP
}

func exactStrings(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range expected {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func canonicalRelativePOSIXPath(value string) bool {
	return value != "" &&
		!strings.Contains(value, `\`) &&
		!strings.HasPrefix(value, "/") &&
		path.Clean(value) == value &&
		value != "." &&
		!strings.HasPrefix(value, "../")
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}
