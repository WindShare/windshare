package browsermatrixpion

import (
	"errors"
	"fmt"
)

const (
	SampleAuthoritySchemaVersion         = "windshare.browser-network-matrix.sample-authority/v1"
	ControlAuthoritySchemaVersion        = "windshare.browser-network-matrix.control-authority/v1"
	AttemptRequestAuthoritySchemaVersion = "windshare.browser-network-matrix.attempt-request-authority/v1"
	AttemptAuthoritySchemaVersion        = "windshare.browser-network-matrix.attempt-authority/v1"

	minimumSampleOrdinal = 1
	maximumSampleOrdinal = 5
)

// SampleAuthority names the single browser process operation allowed to consume
// a control credential. Keeping this authority independent of fixture material
// prevents a valid fixture from being replayed by a sibling browser sample.
type SampleAuthority struct {
	SchemaVersion     string `json:"schemaVersion"`
	RunID             string `json:"runId"`
	ProfileID         string `json:"profileId"`
	Browser           string `json:"browser"`
	SampleOrdinal     int    `json:"sampleOrdinal"`
	ProcessInstanceID string `json:"processInstanceId"`
	OperationID       string `json:"operationId"`
}

// ControlAuthority binds the broker-issued lease to the browser operation that
// requested it, so the lease ID is never treated as self-authenticating context.
type ControlAuthority struct {
	SchemaVersion   string          `json:"schemaVersion"`
	SampleAuthority SampleAuthority `json:"sampleAuthority"`
	ControlLeaseID  string          `json:"controlLeaseId"`
}

// AttemptFixtureBinding contains only immutable signed-fixture identities. The
// sample and control identities live in their own authority layers to avoid
// ambiguous duplicated fields in signed documents.
type AttemptFixtureBinding struct {
	AttestationSHA256       string `json:"attestationSha256"`
	AuthorityInstanceID     string `json:"authorityInstanceId"`
	RemoteServiceInstanceID string `json:"remoteServiceInstanceId"`
	NetworkBindingSHA256    string `json:"networkBindingSha256"`
	RemotePeerBindingSHA256 string `json:"remotePeerBindingSha256"`
}

// AttemptBinding is the pre-attempt combination needed by probe and TURN
// operations. The request and attempt authority layers add their own identities.
type AttemptBinding struct {
	ControlAuthority ControlAuthority      `json:"controlAuthority"`
	FixtureBinding   AttemptFixtureBinding `json:"fixtureBinding"`
}

type AttemptRequestAuthority struct {
	SchemaVersion    string                `json:"schemaVersion"`
	ControlAuthority ControlAuthority      `json:"controlAuthority"`
	RequestID        string                `json:"requestId"`
	FixtureBinding   AttemptFixtureBinding `json:"fixtureBinding"`
}

type AttemptAuthority struct {
	SchemaVersion    string                  `json:"schemaVersion"`
	RequestAuthority AttemptRequestAuthority `json:"requestAuthority"`
	AttemptID        string                  `json:"attemptId"`
	Challenge        string                  `json:"challenge"`
}

func NewSampleAuthority(runID, profileID, browser string, sampleOrdinal int, processInstanceID string) (SampleAuthority, error) {
	authority := SampleAuthority{
		SchemaVersion:     SampleAuthoritySchemaVersion,
		RunID:             runID,
		ProfileID:         profileID,
		Browser:           browser,
		SampleOrdinal:     sampleOrdinal,
		ProcessInstanceID: processInstanceID,
		OperationID:       SampleOperationID(runID, profileID, browser, sampleOrdinal),
	}
	if err := ValidateSampleAuthority(authority); err != nil {
		return SampleAuthority{}, err
	}
	return authority, nil
}

func SampleOperationID(runID, profileID, browser string, sampleOrdinal int) string {
	return fmt.Sprintf("%s-%s-%s-%d", runID, profileID, browser, sampleOrdinal)
}

func ValidateSampleAuthority(authority SampleAuthority) error {
	if authority.SchemaVersion != SampleAuthoritySchemaVersion ||
		validateCanonicalID(authority.RunID, "run ID") != nil ||
		!validProfileID(authority.ProfileID) ||
		!validBrowser(authority.Browser) ||
		authority.SampleOrdinal < minimumSampleOrdinal || authority.SampleOrdinal > maximumSampleOrdinal ||
		validateCanonicalID(authority.ProcessInstanceID, "process instance ID") != nil ||
		authority.OperationID != SampleOperationID(authority.RunID, authority.ProfileID, authority.Browser, authority.SampleOrdinal) {
		return errors.New("sample authority is outside the browser network matrix protocol")
	}
	return nil
}

func ValidateControlAuthority(authority ControlAuthority) error {
	if authority.SchemaVersion != ControlAuthoritySchemaVersion ||
		ValidateSampleAuthority(authority.SampleAuthority) != nil ||
		!validOpaqueID(authority.ControlLeaseID) {
		return errors.New("control authority is outside the browser network matrix protocol")
	}
	return nil
}

func ValidateAttemptFixtureBinding(binding AttemptFixtureBinding) error {
	if !sha256Pattern.MatchString(binding.AttestationSHA256) ||
		validateCanonicalID(binding.AuthorityInstanceID, "authority instance ID") != nil ||
		validateCanonicalID(binding.RemoteServiceInstanceID, "remote service instance ID") != nil ||
		!sha256Pattern.MatchString(binding.NetworkBindingSHA256) ||
		!sha256Pattern.MatchString(binding.RemotePeerBindingSHA256) {
		return errors.New("attempt fixture binding is outside the remote Pion protocol")
	}
	return nil
}

func ValidateAttemptBinding(binding AttemptBinding) error {
	if ValidateControlAuthority(binding.ControlAuthority) != nil ||
		ValidateAttemptFixtureBinding(binding.FixtureBinding) != nil {
		return errors.New("attempt binding is outside the remote Pion protocol")
	}
	return nil
}

func ValidateAttemptRequestAuthority(authority AttemptRequestAuthority) error {
	if authority.SchemaVersion != AttemptRequestAuthoritySchemaVersion ||
		ValidateControlAuthority(authority.ControlAuthority) != nil ||
		!validOpaqueID(authority.RequestID) ||
		authority.RequestID == authority.ControlAuthority.ControlLeaseID ||
		ValidateAttemptFixtureBinding(authority.FixtureBinding) != nil {
		return errors.New("attempt request authority is outside the remote Pion protocol")
	}
	return nil
}

func ValidateAttemptAuthority(authority AttemptAuthority) error {
	if authority.SchemaVersion != AttemptAuthoritySchemaVersion ||
		ValidateAttemptRequestAuthority(authority.RequestAuthority) != nil ||
		!validOpaqueID(authority.AttemptID) || !validOpaqueID(authority.Challenge) {
		return errors.New("attempt authority is outside the remote Pion protocol")
	}
	identities := [...]string{
		authority.RequestAuthority.ControlAuthority.ControlLeaseID,
		authority.RequestAuthority.RequestID,
		authority.AttemptID,
		authority.Challenge,
	}
	for index, identity := range identities {
		for otherIndex := index + 1; otherIndex < len(identities); otherIndex++ {
			if identity == identities[otherIndex] {
				return errors.New("attempt authority identities must be pairwise distinct")
			}
		}
	}
	return nil
}

func CanonicalSampleAuthorityDocument(authority SampleAuthority) ([]byte, error) {
	if err := ValidateSampleAuthority(authority); err != nil {
		return nil, err
	}
	return canonicalJSONLine(authority)
}

func CanonicalControlAuthorityDocument(authority ControlAuthority) ([]byte, error) {
	if err := ValidateControlAuthority(authority); err != nil {
		return nil, err
	}
	return canonicalJSONLine(authority)
}

func CanonicalAttemptRequestAuthorityDocument(authority AttemptRequestAuthority) ([]byte, error) {
	if err := ValidateAttemptRequestAuthority(authority); err != nil {
		return nil, err
	}
	return canonicalJSONLine(authority)
}

func CanonicalAttemptAuthorityDocument(authority AttemptAuthority) ([]byte, error) {
	if err := ValidateAttemptAuthority(authority); err != nil {
		return nil, err
	}
	return canonicalJSONLine(authority)
}

func validBrowser(value string) bool {
	switch value {
	case "chromium", "firefox", "webkit":
		return true
	default:
		return false
	}
}
