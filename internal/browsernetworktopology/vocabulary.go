package browsernetworktopology

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	ManifestSchemaVersion           = "windshare.browser-network-matrix.manifest/v2"
	ProfileSchemaVersion            = "windshare.browser-network-matrix.scheduled-profile/v2"
	RuntimeAttestationSchemaVersion = "windshare.browser-network-matrix.runtime-attestation/v3"
	SampleResultSchemaVersion       = "windshare.browser-network-matrix.scheduled-sample/v2"
	RunResultSchemaVersion          = "windshare.browser-network-matrix.scheduled-run/v2"
	AggregateSchemaVersion          = "windshare.browser-network-matrix.scheduled-verdict/v2"

	MatrixID                    = "scheduled-hard-browser-network-v1"
	ScheduledHardFailClosed     = "scheduled-hard-fail-closed"
	SamplesPerBrowser           = 5
	ScheduledIdentityCount      = 45
	TotalIdentityCount          = ScheduledIdentityCount
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
)

type ProfileKind string

const (
	ProfileScheduledPublicSTUN    ProfileKind = "scheduled-public-stun"
	ProfileScheduledRestrictedUDP ProfileKind = "scheduled-restricted-udp"
	ProfileScheduledCoturn        ProfileKind = "scheduled-coturn"
)

type AuthorityKind string

const (
	AuthorityExternalFixture AuthorityKind = "external-fixture"
)

const AvailabilityRequired = "required"

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
		profilePath:   "profiles/scheduled-public-stun.v2.json",
		connectivity:  ConnectivityEstablished,
	},
	{
		profileID:     string(ProfileScheduledRestrictedUDP),
		kind:          ProfileScheduledRestrictedUDP,
		executionMode: ModeScheduled,
		authorityID:   "restricted-udp-external-fixture",
		authorityKind: AuthorityExternalFixture,
		profilePath:   "profiles/scheduled-restricted-udp.v2.json",
		connectivity:  ConnectivityBlocked,
	},
	{
		profileID:     string(ProfileScheduledCoturn),
		kind:          ProfileScheduledCoturn,
		executionMode: ModeScheduled,
		authorityID:   "coturn-external-fixture",
		authorityKind: AuthorityExternalFixture,
		profilePath:   "profiles/scheduled-coturn.v2.json",
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
	return mode == ModeScheduled
}

func validBrowser(browser Browser) bool {
	return browser == BrowserChromium || browser == BrowserFirefox || browser == BrowserWebKit
}

func validCandidateType(candidateType CandidateType) bool {
	return slices.Contains(candidateTypeOrder, candidateType)
}

func validProtocol(protocol TransportProtocol) bool {
	return protocol == ProtocolUDP || protocol == ProtocolTCP
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
