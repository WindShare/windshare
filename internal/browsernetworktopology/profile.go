package browsernetworktopology

import (
	"errors"
	"fmt"
	"slices"
)

var (
	ErrInvalidProfile         = errors.New("invalid browser network matrix profile")
	ErrInvalidCandidatePolicy = errors.New("invalid candidate-path acceptance policy")
)

type AuthorityReference struct {
	AuthorityID                string        `json:"authorityId"`
	AuthorityKind              AuthorityKind `json:"authorityKind"`
	AvailabilityExpectation    string        `json:"availabilityExpectation"`
	AttestationPublicKeySHA256 string        `json:"attestationPublicKeySha256"`
}

type CandidateTypeConstraint struct {
	Allowed   []CandidateType `json:"allowed"`
	Required  []CandidateType `json:"required"`
	Forbidden []CandidateType `json:"forbidden"`
}

type ProtocolConstraint struct {
	Allowed   []TransportProtocol `json:"allowed"`
	Required  []TransportProtocol `json:"required"`
	Forbidden []TransportProtocol `json:"forbidden"`
}

type CandidatePolicy struct {
	SelectedPair         SelectedPairRequirement `json:"selectedPair"`
	LocalCandidateTypes  CandidateTypeConstraint `json:"localCandidateTypes"`
	RemoteCandidateTypes CandidateTypeConstraint `json:"remoteCandidateTypes"`
	Protocols            ProtocolConstraint      `json:"protocols"`
}

type Profile struct {
	SchemaVersion           string                  `json:"schemaVersion"`
	ProfileID               string                  `json:"profileId"`
	ProfileKind             ProfileKind             `json:"profileKind"`
	ExecutionMode           ExecutionMode           `json:"executionMode"`
	Authority               AuthorityReference      `json:"authority"`
	ConnectivityExpectation ConnectivityExpectation `json:"connectivityExpectation"`
	CandidatePolicy         CandidatePolicy         `json:"candidatePolicy"`
}

func ParseProfile(encoded []byte) (Profile, error) {
	var profile Profile
	if err := decodeCanonicalDocument(encoded, "browser network matrix profile", &profile, ErrInvalidProfile); err != nil {
		return Profile{}, err
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func (profile Profile) Validate() error {
	spec, known := findProfileSpec(profile.ProfileID)
	if !known || profile.SchemaVersion != ProfileSchemaVersion ||
		profile.ProfileKind != spec.kind || profile.ExecutionMode != spec.executionMode ||
		profile.ConnectivityExpectation != spec.connectivity {
		return fmt.Errorf("%w: schema, profile identity, kind, mode, or connectivity differs", ErrInvalidProfile)
	}
	if profile.Authority.AuthorityID != spec.authorityID ||
		profile.Authority.AuthorityKind != spec.authorityKind ||
		profile.Authority.AvailabilityExpectation != AvailabilityNotAssumed ||
		!isCanonicalSHA256(profile.Authority.AttestationPublicKeySHA256) {
		return fmt.Errorf("%w: authority binding differs for profile %q", ErrInvalidProfile, profile.ProfileID)
	}
	if err := profile.CandidatePolicy.Validate(profile.ConnectivityExpectation); err != nil {
		return errors.Join(ErrInvalidProfile, err)
	}
	return nil
}

func (policy CandidatePolicy) Validate(expectation ConnectivityExpectation) error {
	switch policy.SelectedPair {
	case SelectedPairRequired:
		if expectation != ConnectivityEstablished {
			return fmt.Errorf("%w: a required pair needs established connectivity", ErrInvalidCandidatePolicy)
		}
	case SelectedPairProhibited:
		if expectation != ConnectivityBlocked {
			return fmt.Errorf("%w: a prohibited pair needs blocked connectivity", ErrInvalidCandidatePolicy)
		}
	default:
		return fmt.Errorf("%w: selected-pair requirement is unknown", ErrInvalidCandidatePolicy)
	}

	if err := validateCandidateTypeConstraint("local", policy.LocalCandidateTypes); err != nil {
		return err
	}
	if err := validateCandidateTypeConstraint("remote", policy.RemoteCandidateTypes); err != nil {
		return err
	}
	if err := validateProtocolConstraint(policy.Protocols); err != nil {
		return err
	}

	if policy.SelectedPair == SelectedPairProhibited {
		if !emptyCandidateConstraint(policy.LocalCandidateTypes) ||
			!emptyCandidateConstraint(policy.RemoteCandidateTypes) ||
			!emptyProtocolConstraint(policy.Protocols) {
			return fmt.Errorf("%w: prohibited selected pairs cannot carry path constraints", ErrInvalidCandidatePolicy)
		}
		return nil
	}
	if len(policy.LocalCandidateTypes.Allowed) == 0 ||
		len(policy.RemoteCandidateTypes.Allowed) == 0 ||
		len(policy.Protocols.Allowed) == 0 {
		return fmt.Errorf("%w: required selected pairs need non-empty allowed sets", ErrInvalidCandidatePolicy)
	}
	return nil
}

func validateCandidateTypeConstraint(side string, constraint CandidateTypeConstraint) error {
	if constraint.Allowed == nil || constraint.Required == nil || constraint.Forbidden == nil {
		return fmt.Errorf("%w: %s candidate sets must be arrays", ErrInvalidCandidatePolicy, side)
	}
	if !canonicalCandidateTypes(constraint.Allowed) ||
		!canonicalCandidateTypes(constraint.Required) ||
		!canonicalCandidateTypes(constraint.Forbidden) {
		return fmt.Errorf("%w: %s candidate sets contain unknown, duplicate, or unordered values", ErrInvalidCandidatePolicy, side)
	}
	if len(constraint.Required) > 1 {
		return fmt.Errorf("%w: %s has more than one required value for a single selected pair", ErrInvalidCandidatePolicy, side)
	}
	if !candidateSubset(constraint.Required, constraint.Allowed) ||
		candidateOverlap(constraint.Allowed, constraint.Forbidden) {
		return fmt.Errorf("%w: %s candidate sets contradict each other", ErrInvalidCandidatePolicy, side)
	}
	if len(constraint.Allowed) > 0 && !allCandidateTypesClassified(constraint.Allowed, constraint.Forbidden) {
		return fmt.Errorf("%w: %s candidate policy does not classify the frozen vocabulary", ErrInvalidCandidatePolicy, side)
	}
	return nil
}

func validateProtocolConstraint(constraint ProtocolConstraint) error {
	if constraint.Allowed == nil || constraint.Required == nil || constraint.Forbidden == nil {
		return fmt.Errorf("%w: protocol sets must be arrays", ErrInvalidCandidatePolicy)
	}
	if !canonicalProtocols(constraint.Allowed) ||
		!canonicalProtocols(constraint.Required) ||
		!canonicalProtocols(constraint.Forbidden) {
		return fmt.Errorf("%w: protocol sets contain unknown, duplicate, or unordered values", ErrInvalidCandidatePolicy)
	}
	if len(constraint.Required) > 1 {
		return fmt.Errorf("%w: a selected pair cannot require multiple protocols", ErrInvalidCandidatePolicy)
	}
	if !protocolSubset(constraint.Required, constraint.Allowed) ||
		protocolOverlap(constraint.Allowed, constraint.Forbidden) {
		return fmt.Errorf("%w: protocol sets contradict each other", ErrInvalidCandidatePolicy)
	}
	if len(constraint.Allowed) > 0 && !allProtocolsClassified(constraint.Allowed, constraint.Forbidden) {
		return fmt.Errorf("%w: protocol policy does not classify the frozen vocabulary", ErrInvalidCandidatePolicy)
	}
	return nil
}

func allCandidateTypesClassified(allowed, forbidden []CandidateType) bool {
	for _, candidateType := range candidateTypeOrder {
		if !containsCandidateType(allowed, candidateType) && !containsCandidateType(forbidden, candidateType) {
			return false
		}
	}
	return true
}

func allProtocolsClassified(allowed, forbidden []TransportProtocol) bool {
	for _, protocol := range protocolOrder {
		if !containsProtocol(allowed, protocol) && !containsProtocol(forbidden, protocol) {
			return false
		}
	}
	return true
}

func canonicalCandidateTypes(values []CandidateType) bool {
	if values == nil {
		return false
	}
	expected := make([]CandidateType, 0, len(values))
	for _, candidateType := range candidateTypeOrder {
		if containsCandidateType(values, candidateType) {
			expected = append(expected, candidateType)
		}
	}
	if len(expected) != len(values) {
		return false
	}
	for index := range values {
		if values[index] != expected[index] {
			return false
		}
	}
	return true
}

func canonicalProtocols(values []TransportProtocol) bool {
	if values == nil {
		return false
	}
	expected := make([]TransportProtocol, 0, len(values))
	for _, protocol := range protocolOrder {
		if containsProtocol(values, protocol) {
			expected = append(expected, protocol)
		}
	}
	if len(expected) != len(values) {
		return false
	}
	for index := range values {
		if values[index] != expected[index] {
			return false
		}
	}
	return true
}

func candidateSubset(subset, set []CandidateType) bool {
	for _, value := range subset {
		if !containsCandidateType(set, value) {
			return false
		}
	}
	return true
}

func protocolSubset(subset, set []TransportProtocol) bool {
	for _, value := range subset {
		if !containsProtocol(set, value) {
			return false
		}
	}
	return true
}

func candidateOverlap(left, right []CandidateType) bool {
	for _, value := range left {
		if containsCandidateType(right, value) {
			return true
		}
	}
	return false
}

func protocolOverlap(left, right []TransportProtocol) bool {
	for _, value := range left {
		if containsProtocol(right, value) {
			return true
		}
	}
	return false
}

func containsCandidateType(values []CandidateType, expected CandidateType) bool {
	return slices.Contains(values, expected)
}

func containsProtocol(values []TransportProtocol, expected TransportProtocol) bool {
	return slices.Contains(values, expected)
}

func emptyCandidateConstraint(constraint CandidateTypeConstraint) bool {
	return len(constraint.Allowed) == 0 && len(constraint.Required) == 0 && len(constraint.Forbidden) == 0
}

func emptyProtocolConstraint(constraint ProtocolConstraint) bool {
	return len(constraint.Allowed) == 0 && len(constraint.Required) == 0 && len(constraint.Forbidden) == 0
}

func (profile Profile) CanonicalJSON() ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return marshalCanonicalDocument(profile)
}

func (profile Profile) SHA256() (string, error) {
	encoded, err := profile.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return sha256Text(encoded), nil
}

func cloneProfile(profile Profile) Profile {
	profile.CandidatePolicy.LocalCandidateTypes = cloneCandidateTypeConstraint(
		profile.CandidatePolicy.LocalCandidateTypes,
	)
	profile.CandidatePolicy.RemoteCandidateTypes = cloneCandidateTypeConstraint(
		profile.CandidatePolicy.RemoteCandidateTypes,
	)
	profile.CandidatePolicy.Protocols = cloneProtocolConstraint(profile.CandidatePolicy.Protocols)
	return profile
}

func cloneCandidateTypeConstraint(constraint CandidateTypeConstraint) CandidateTypeConstraint {
	constraint.Allowed = cloneCandidateTypes(constraint.Allowed)
	constraint.Required = cloneCandidateTypes(constraint.Required)
	constraint.Forbidden = cloneCandidateTypes(constraint.Forbidden)
	return constraint
}

func cloneProtocolConstraint(constraint ProtocolConstraint) ProtocolConstraint {
	constraint.Allowed = cloneProtocols(constraint.Allowed)
	constraint.Required = cloneProtocols(constraint.Required)
	constraint.Forbidden = cloneProtocols(constraint.Forbidden)
	return constraint
}

func cloneCandidateTypes(values []CandidateType) []CandidateType {
	if values == nil {
		return nil
	}
	return append([]CandidateType{}, values...)
}

func cloneProtocols(values []TransportProtocol) []TransportProtocol {
	if values == nil {
		return nil
	}
	return append([]TransportProtocol{}, values...)
}
