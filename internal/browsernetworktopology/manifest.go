package browsernetworktopology

import (
	"errors"
	"fmt"
)

var ErrInvalidManifest = errors.New("invalid browser network matrix manifest")

type AuthorityDefinition struct {
	AuthorityID                string        `json:"authorityId"`
	AuthorityKind              AuthorityKind `json:"authorityKind"`
	AvailabilityExpectation    string        `json:"availabilityExpectation"`
	AttestationPublicKeySHA256 string        `json:"attestationPublicKeySha256"`
}

type ProfileReference struct {
	ProfileID     string        `json:"profileId"`
	ProfileKind   ProfileKind   `json:"profileKind"`
	ExecutionMode ExecutionMode `json:"executionMode"`
	AuthorityID   string        `json:"authorityId"`
	AuthorityKind AuthorityKind `json:"authorityKind"`
	ProfilePath   string        `json:"profilePath"`
	ProfileSHA256 string        `json:"profileSha256"`
}

type IdentityCounts struct {
	Total     int `json:"total"`
	Scheduled int `json:"scheduled"`
	Manual    int `json:"manual"`
}

type Manifest struct {
	SchemaVersion      string                `json:"schemaVersion"`
	MatrixID           string                `json:"matrixId"`
	ReportingSemantics string                `json:"reportingSemantics"`
	Browsers           []Browser             `json:"browsers"`
	SampleOrdinals     []int                 `json:"sampleOrdinals"`
	Authorities        []AuthorityDefinition `json:"authorities"`
	Profiles           []ProfileReference    `json:"profiles"`
	IdentityCounts     IdentityCounts        `json:"identityCounts"`
}

type ProfileDocument struct {
	Path string
	JSON []byte
}

// Contract owns the cross-document bindings so callers cannot accidentally
// validate evidence against a profile from a different manifest generation.
type Contract struct {
	manifest          Manifest
	manifestSHA256    string
	profiles          []Profile
	profileSHA256ByID []string
}

type SampleIdentity struct {
	ProfileID     string  `json:"profileId"`
	Browser       Browser `json:"browser"`
	SampleOrdinal int     `json:"sampleOrdinal"`
}

func ParseManifest(encoded []byte) (Manifest, error) {
	var manifest Manifest
	if err := decodeCanonicalDocument(encoded, "browser network matrix manifest", &manifest, ErrInvalidManifest); err != nil {
		return Manifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.MatrixID != MatrixID ||
		manifest.ReportingSemantics != ObservationalNonblocking {
		return fmt.Errorf("%w: schema, matrix identity, or reporting semantics differs", ErrInvalidManifest)
	}
	if !exactBrowsers(manifest.Browsers, frozenBrowsers) ||
		!exactInts(manifest.SampleOrdinals, frozenSampleOrdinals) {
		return fmt.Errorf("%w: browser or sample universe differs", ErrInvalidManifest)
	}
	if len(manifest.Authorities) != len(frozenProfileSpecs) || len(manifest.Profiles) != len(frozenProfileSpecs) {
		return fmt.Errorf("%w: authority or profile registry size differs", ErrInvalidManifest)
	}
	seenProfileDigests := make(map[string]struct{}, len(manifest.Profiles))
	for index, spec := range frozenProfileSpecs {
		authority := manifest.Authorities[index]
		if authority.AuthorityID != spec.authorityID || authority.AuthorityKind != spec.authorityKind ||
			authority.AvailabilityExpectation != AvailabilityNotAssumed ||
			!isCanonicalSHA256(authority.AttestationPublicKeySHA256) {
			return fmt.Errorf("%w: authority %d differs from its fixed identity", ErrInvalidManifest, index)
		}

		profile := manifest.Profiles[index]
		if profile.ProfileID != spec.profileID || profile.ProfileKind != spec.kind ||
			profile.ExecutionMode != spec.executionMode || profile.AuthorityID != spec.authorityID ||
			profile.AuthorityKind != spec.authorityKind || profile.ProfilePath != spec.profilePath ||
			!canonicalRelativePOSIXPath(profile.ProfilePath) || !isCanonicalSHA256(profile.ProfileSHA256) {
			return fmt.Errorf("%w: profile reference %d differs from its fixed identity or path", ErrInvalidManifest, index)
		}
		if _, duplicate := seenProfileDigests[profile.ProfileSHA256]; duplicate {
			return fmt.Errorf("%w: profile reference %d reuses another profile digest", ErrInvalidManifest, index)
		}
		seenProfileDigests[profile.ProfileSHA256] = struct{}{}
	}
	if manifest.IdentityCounts != (IdentityCounts{
		Total: TotalIdentityCount, Scheduled: ScheduledIdentityCount, Manual: ManualIdentityCount,
	}) {
		return fmt.Errorf("%w: identity counts differ", ErrInvalidManifest)
	}
	return nil
}

func (manifest Manifest) CanonicalJSON() ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return marshalCanonicalDocument(manifest)
}

func (manifest Manifest) SHA256() (string, error) {
	encoded, err := manifest.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return sha256Text(encoded), nil
}

func ParseContract(manifestJSON []byte, profileDocuments []ProfileDocument) (Contract, error) {
	manifest, err := ParseManifest(manifestJSON)
	if err != nil {
		return Contract{}, err
	}
	if len(profileDocuments) != len(manifest.Profiles) {
		return Contract{}, fmt.Errorf("%w: profile document count differs", ErrInvalidManifest)
	}

	profiles := make([]Profile, len(profileDocuments))
	digests := make([]string, len(profileDocuments))
	for index, document := range profileDocuments {
		reference := manifest.Profiles[index]
		if !canonicalRelativePOSIXPath(document.Path) || document.Path != reference.ProfilePath {
			return Contract{}, fmt.Errorf("%w: profile document %d path differs", ErrInvalidManifest, index)
		}
		profile, parseErr := ParseProfile(document.JSON)
		if parseErr != nil {
			return Contract{}, errors.Join(ErrInvalidManifest, parseErr)
		}
		digest, digestErr := profile.SHA256()
		if digestErr != nil {
			return Contract{}, errors.Join(ErrInvalidManifest, digestErr)
		}
		if digest != reference.ProfileSHA256 || profile.ProfileID != reference.ProfileID ||
			profile.ProfileKind != reference.ProfileKind || profile.ExecutionMode != reference.ExecutionMode ||
			profile.Authority.AuthorityID != reference.AuthorityID ||
			profile.Authority.AuthorityKind != reference.AuthorityKind ||
			profile.Authority.AttestationPublicKeySHA256 !=
				manifest.Authorities[index].AttestationPublicKeySHA256 {
			return Contract{}, fmt.Errorf("%w: profile document %d digest or binding differs", ErrInvalidManifest, index)
		}
		profiles[index] = profile
		digests[index] = digest
	}

	manifestDigest, err := manifest.SHA256()
	if err != nil {
		return Contract{}, err
	}
	return Contract{
		manifest:          manifest,
		manifestSHA256:    manifestDigest,
		profiles:          profiles,
		profileSHA256ByID: digests,
	}, nil
}

func (contract Contract) ManifestSHA256() string {
	return contract.manifestSHA256
}

func (contract Contract) MatrixID() string {
	return contract.manifest.MatrixID
}

func (contract Contract) ReportingSemantics() string {
	return contract.manifest.ReportingSemantics
}

func (contract Contract) Profile(profileID string) (Profile, string, bool) {
	for index, profile := range contract.profiles {
		if profile.ProfileID == profileID {
			return cloneProfile(profile), contract.profileSHA256ByID[index], true
		}
	}
	return Profile{}, "", false
}

func (contract Contract) ExpectedIdentities(mode ExecutionMode) ([]SampleIdentity, error) {
	if !validExecutionMode(mode) || !isCanonicalSHA256(contract.manifestSHA256) {
		return nil, fmt.Errorf("%w: contract or execution mode is invalid", ErrInvalidManifest)
	}
	identities := make([]SampleIdentity, 0, identitiesForMode(mode))
	for _, reference := range contract.manifest.Profiles {
		if reference.ExecutionMode != mode {
			continue
		}
		for _, browser := range contract.manifest.Browsers {
			for _, sampleOrdinal := range contract.manifest.SampleOrdinals {
				identities = append(identities, SampleIdentity{
					ProfileID: reference.ProfileID, Browser: browser, SampleOrdinal: sampleOrdinal,
				})
			}
		}
	}
	if len(identities) != identitiesForMode(mode) {
		return nil, fmt.Errorf("%w: derived identity universe differs", ErrInvalidManifest)
	}
	return identities, nil
}

func (contract Contract) AllExpectedIdentities() ([]SampleIdentity, error) {
	scheduled, err := contract.ExpectedIdentities(ModeScheduled)
	if err != nil {
		return nil, err
	}
	manual, err := contract.ExpectedIdentities(ModeManual)
	if err != nil {
		return nil, err
	}
	return append(scheduled, manual...), nil
}

func (contract Contract) profileIDs(mode ExecutionMode) []string {
	profileIDs := make([]string, 0, len(contract.profiles))
	for _, profile := range contract.profiles {
		if profile.ExecutionMode == mode {
			profileIDs = append(profileIDs, profile.ProfileID)
		}
	}
	return profileIDs
}

func identitiesForMode(mode ExecutionMode) int {
	if mode == ModeScheduled {
		return ScheduledIdentityCount
	}
	if mode == ModeManual {
		return ManualIdentityCount
	}
	return 0
}

func exactBrowsers(actual, expected []Browser) bool {
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

func exactInts(actual, expected []int) bool {
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

func exactIdentities(actual, expected []SampleIdentity) bool {
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
