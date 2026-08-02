package browsernetworktopology

import (
	"errors"
	"fmt"
)

const (
	ManualSupplementProfileSchemaVersion = "windshare.browser-network-matrix.manual-supplement-profile/v1"
	ManualSupplementID                   = "manual-real-nat-supplement-v1"
	ManualSupplementReportingSemantics   = "manual-supplemental-non-authoritative"
	ManualRealNATProfileID               = "manual-real-nat"
	ManualRealNATAuthorityID             = "real-nat-external-fixture"
	AvailabilityOperatorProvisioned      = "operator-provisioned"
)

var ErrInvalidManualSupplement = errors.New("invalid browser network manual supplement")

// ManualSupplementProfile is deliberately not a Profile. Keeping the manual
// topology outside the scheduled type prevents it from entering hard counts,
// runtime inputs, or verdict construction through an innocent shared helper.
type ManualSupplementProfile struct {
	SchemaVersion           string                  `json:"schemaVersion"`
	SupplementID            string                  `json:"supplementId"`
	ReportingSemantics      string                  `json:"reportingSemantics"`
	ProfileID               string                  `json:"profileId"`
	Authority               AuthorityReference      `json:"authority"`
	ConnectivityExpectation ConnectivityExpectation `json:"connectivityExpectation"`
	CandidatePolicy         CandidatePolicy         `json:"candidatePolicy"`
}

func ParseManualSupplementProfile(encoded []byte) (ManualSupplementProfile, error) {
	var profile ManualSupplementProfile
	if err := decodeCanonicalDocument(
		encoded,
		"browser network manual supplement",
		&profile,
		ErrInvalidManualSupplement,
	); err != nil {
		return ManualSupplementProfile{}, err
	}
	if err := profile.Validate(); err != nil {
		return ManualSupplementProfile{}, err
	}
	return profile, nil
}

func (profile ManualSupplementProfile) Validate() error {
	if profile.SchemaVersion != ManualSupplementProfileSchemaVersion ||
		profile.SupplementID != ManualSupplementID ||
		profile.ReportingSemantics != ManualSupplementReportingSemantics ||
		profile.ProfileID != ManualRealNATProfileID ||
		profile.ConnectivityExpectation != ConnectivityEstablished {
		return fmt.Errorf("%w: identity or reporting boundary differs", ErrInvalidManualSupplement)
	}
	if profile.Authority.AuthorityID != ManualRealNATAuthorityID ||
		profile.Authority.AuthorityKind != AuthorityExternalFixture ||
		profile.Authority.AvailabilityExpectation != AvailabilityOperatorProvisioned ||
		!isCanonicalSHA256(profile.Authority.AttestationPublicKeySHA256) {
		return fmt.Errorf("%w: operator authority binding differs", ErrInvalidManualSupplement)
	}
	if err := profile.CandidatePolicy.Validate(profile.ConnectivityExpectation); err != nil {
		return errors.Join(ErrInvalidManualSupplement, err)
	}
	return nil
}

func (profile ManualSupplementProfile) CanonicalJSON() ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return marshalCanonicalDocument(profile)
}
