package browsernetworktopology

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidSampleResult = errors.New("invalid browser network matrix sample result")
	ErrInvalidRunResult    = errors.New("invalid browser network matrix run result")
)

type SampleOutcome string

const (
	SampleObserved             SampleOutcome = "observed"
	SampleInfrastructureFailed SampleOutcome = "infrastructure-failed"
)

type SampleFailureCode string

const (
	FailureSampleRunner             SampleFailureCode = "sample-runner-failed"
	FailureSampleDeadline           SampleFailureCode = "sample-deadline-exceeded"
	FailureSampleEvidenceCollection SampleFailureCode = "evidence-collection-failed"
)

type SampleFailure struct {
	FailureCode SampleFailureCode `json:"failureCode"`
}

type SampleResult struct {
	SchemaVersion          string                   `json:"schemaVersion"`
	RunID                  string                   `json:"runId"`
	ManifestSHA256         string                   `json:"manifestSha256"`
	Identity               SampleIdentity           `json:"identity"`
	ProfileSHA256          string                   `json:"profileSha256"`
	AttestationSHA256      string                   `json:"attestationSha256"`
	SampleOutcome          SampleOutcome            `json:"sampleOutcome"`
	ProcessInstanceID      *string                  `json:"processInstanceId"`
	AttemptEvidence        *AttemptEvidence         `json:"attemptEvidence"`
	CandidatePolicyOutcome CandidatePolicyOutcome   `json:"candidatePolicyOutcome"`
	RationaleCodes         []CandidateRationaleCode `json:"rationaleCodes"`
	Failure                *SampleFailure           `json:"failure"`
}

type OrchestrationOutcome string

const (
	OrchestrationHealthy OrchestrationOutcome = "healthy"
	OrchestrationFailed  OrchestrationOutcome = "failed"
)

type OrchestrationFailureCode string

const (
	FailureRuntimeBootstrap     OrchestrationFailureCode = "runtime-bootstrap-failed"
	FailureOrchestratorDeadline OrchestrationFailureCode = "orchestrator-deadline-exceeded"
	FailureCollector            OrchestrationFailureCode = "collector-failed"
	FailureContainmentCleanup   OrchestrationFailureCode = "containment-cleanup-failed"
)

type OrchestrationFailure struct {
	FailureCode OrchestrationFailureCode `json:"failureCode"`
}

type RunOutcome string

const (
	RunCompleted            RunOutcome = "completed"
	RunPartial              RunOutcome = "partial"
	RunNotExecuted          RunOutcome = "not-executed"
	RunInfrastructureFailed RunOutcome = "infrastructure-failed"
)

type ProfileOutcome string

const (
	ProfileCompleted            ProfileOutcome = "completed"
	ProfileNotExecuted          ProfileOutcome = "not-executed"
	ProfileInfrastructureFailed ProfileOutcome = "infrastructure-failed"
)

// ProfileRunResult preserves topology-level execution truth independently from
// candidate-policy observations, so a mismatch cannot masquerade as missing evidence.
type ProfileRunResult struct {
	ProfileID                    string              `json:"profileId"`
	PrerequisiteOutcome          PrerequisiteOutcome `json:"prerequisiteOutcome"`
	ExpectedSamples              int                 `json:"expectedSamples"`
	ObservedSamples              int                 `json:"observedSamples"`
	SampleInfrastructureFailures int                 `json:"sampleInfrastructureFailures"`
	ProfileOutcome               ProfileOutcome      `json:"profileOutcome"`
}

type RunResult struct {
	SchemaVersion        string                `json:"schemaVersion"`
	RunID                string                `json:"runId"`
	ManifestSHA256       string                `json:"manifestSha256"`
	ExecutionMode        ExecutionMode         `json:"executionMode"`
	OrchestrationOutcome OrchestrationOutcome  `json:"orchestrationOutcome"`
	OrchestrationFailure *OrchestrationFailure `json:"orchestrationFailure"`
	ExpectedIdentities   []SampleIdentity      `json:"expectedIdentities"`
	RuntimeAttestations  []RuntimeAttestation  `json:"runtimeAttestations"`
	Samples              []SampleResult        `json:"samples"`
	ProfileResults       []ProfileRunResult    `json:"profileResults"`
	RunOutcome           RunOutcome            `json:"runOutcome"`
}

func ParseSampleResult(
	encoded []byte,
	contract Contract,
	attestation RuntimeAttestation,
) (SampleResult, error) {
	var sample SampleResult
	if err := decodeCanonicalDocument(encoded, "browser network matrix sample result", &sample, ErrInvalidSampleResult); err != nil {
		return SampleResult{}, err
	}
	if err := sample.Validate(contract, attestation); err != nil {
		return SampleResult{}, err
	}
	return sample, nil
}

func (sample SampleResult) Validate(contract Contract, attestation RuntimeAttestation) error {
	profile, profileDigest, known := contract.Profile(sample.Identity.ProfileID)
	if !known || sample.SchemaVersion != SampleResultSchemaVersion || !validIdentifier(sample.RunID) ||
		sample.ManifestSHA256 != contract.ManifestSHA256() || sample.ProfileSHA256 != profileDigest ||
		!validBrowser(sample.Identity.Browser) || sample.Identity.SampleOrdinal < 1 ||
		sample.Identity.SampleOrdinal > SamplesPerBrowser {
		return fmt.Errorf("%w: schema, run, manifest, identity, or profile binding differs", ErrInvalidSampleResult)
	}
	if err := attestation.Validate(contract); err != nil {
		return errors.Join(ErrInvalidSampleResult, err)
	}
	attestationDigest, err := attestation.SHA256(contract)
	if err != nil || attestation.RunID != sample.RunID || attestation.ProfileID != sample.Identity.ProfileID ||
		attestation.PrerequisiteOutcome != PrerequisiteSatisfied || sample.AttestationSHA256 != attestationDigest {
		return fmt.Errorf("%w: runtime attestation binding differs or is not satisfied", ErrInvalidSampleResult)
	}
	if sample.RationaleCodes == nil {
		return fmt.Errorf("%w: rationaleCodes must be an array", ErrInvalidSampleResult)
	}

	switch sample.SampleOutcome {
	case SampleObserved:
		if sample.ProcessInstanceID == nil || !validIdentifier(*sample.ProcessInstanceID) ||
			sample.AttemptEvidence == nil || sample.Failure != nil {
			return fmt.Errorf("%w: observed sample needs process and attempt authorities and cannot carry infrastructure failure", ErrInvalidSampleResult)
		}
		if attestation.Proof == nil {
			return fmt.Errorf("%w: observed sample lacks its satisfied fixture proof", ErrInvalidSampleResult)
		}
		if err := sample.AttemptEvidence.Validate(
			sample.Identity.ProfileID,
			attestation.Proof.ExternalFixtureTrust,
		); err != nil {
			return errors.Join(ErrInvalidSampleResult, err)
		}
		outcome, rationales, evaluationErr := EvaluateCandidatePath(
			profile.CandidatePolicy,
			profile.ConnectivityExpectation,
			sample.AttemptEvidence.BrowserSelectedPair,
		)
		if evaluationErr != nil || sample.CandidatePolicyOutcome != outcome ||
			!exactRationaleCodes(sample.RationaleCodes, rationales) {
			return fmt.Errorf("%w: candidate policy evaluation is invalid or was not derived", ErrInvalidSampleResult)
		}
	case SampleInfrastructureFailed:
		if sample.ProcessInstanceID != nil || sample.AttemptEvidence != nil ||
			sample.CandidatePolicyOutcome != CandidatePolicyNotEvaluated ||
			len(sample.RationaleCodes) != 0 || sample.Failure == nil || !validSampleFailure(sample.Failure.FailureCode) {
			return fmt.Errorf("%w: infrastructure-failed sample carries attempt proof or lacks typed failure", ErrInvalidSampleResult)
		}
	default:
		return fmt.Errorf("%w: sample outcome is unknown", ErrInvalidSampleResult)
	}
	return nil
}

func validSampleFailure(code SampleFailureCode) bool {
	return code == FailureSampleRunner || code == FailureSampleDeadline || code == FailureSampleEvidenceCollection
}

func (sample SampleResult) CanonicalJSON(
	contract Contract,
	attestation RuntimeAttestation,
) ([]byte, error) {
	if err := sample.Validate(contract, attestation); err != nil {
		return nil, err
	}
	return marshalCanonicalDocument(sample)
}

func (sample SampleResult) SHA256(contract Contract, attestation RuntimeAttestation) (string, error) {
	encoded, err := sample.CanonicalJSON(contract, attestation)
	if err != nil {
		return "", err
	}
	return sha256Text(encoded), nil
}

func ParseRunResult(encoded []byte, contract Contract) (RunResult, error) {
	var result RunResult
	if err := decodeCanonicalDocument(encoded, "browser network matrix run result", &result, ErrInvalidRunResult); err != nil {
		return RunResult{}, err
	}
	if err := result.Validate(contract); err != nil {
		return RunResult{}, err
	}
	return result, nil
}

func (result RunResult) Validate(contract Contract) error {
	if result.SchemaVersion != RunResultSchemaVersion || !validIdentifier(result.RunID) ||
		result.ManifestSHA256 != contract.ManifestSHA256() || !validExecutionMode(result.ExecutionMode) {
		return fmt.Errorf("%w: schema, run, manifest, or mode binding differs", ErrInvalidRunResult)
	}
	expectedIdentities, err := contract.ExpectedIdentities(result.ExecutionMode)
	if err != nil || !exactIdentities(result.ExpectedIdentities, expectedIdentities) {
		return fmt.Errorf("%w: expected identity universe is missing, duplicated, extra, or unordered", ErrInvalidRunResult)
	}

	profileIDs := contract.profileIDs(result.ExecutionMode)
	if result.RuntimeAttestations == nil || len(result.RuntimeAttestations) != len(profileIDs) || result.Samples == nil {
		return fmt.Errorf("%w: runtime attestations or samples do not have array shape", ErrInvalidRunResult)
	}
	for index, profileID := range profileIDs {
		attestation := result.RuntimeAttestations[index]
		if attestation.ProfileID != profileID || attestation.RunID != result.RunID {
			return fmt.Errorf("%w: runtime attestations are missing, duplicated, or unordered", ErrInvalidRunResult)
		}
		if err := attestation.Validate(contract); err != nil {
			return errors.Join(ErrInvalidRunResult, err)
		}
	}

	sampleCounts := make([]int, len(profileIDs))
	processInstanceIDs := make(map[string]struct{})
	attemptIDs := make(map[string]struct{})
	challengeBindings := make(map[string]struct{})
	previousIdentityIndex := -1
	for sampleIndex, sample := range result.Samples {
		identityIndex := findIdentityIndex(expectedIdentities, sample.Identity)
		if identityIndex < 0 || identityIndex <= previousIdentityIndex {
			return fmt.Errorf("%w: sample %d is unexpected, duplicate, or unordered", ErrInvalidRunResult, sampleIndex)
		}
		previousIdentityIndex = identityIndex
		profileIndex := findStringIndex(profileIDs, sample.Identity.ProfileID)
		if profileIndex < 0 ||
			result.RuntimeAttestations[profileIndex].PrerequisiteOutcome != PrerequisiteSatisfied {
			return fmt.Errorf("%w: unsatisfied profile %q carries a sample", ErrInvalidRunResult, sample.Identity.ProfileID)
		}
		if err := sample.Validate(contract, result.RuntimeAttestations[profileIndex]); err != nil {
			return errors.Join(ErrInvalidRunResult, err)
		}
		if sample.SampleOutcome == SampleObserved {
			if repeatedObservedAuthority(
				sample,
				processInstanceIDs,
				attemptIDs,
				challengeBindings,
			) {
				return fmt.Errorf("%w: observed samples reuse process, attempt, or challenge authority", ErrInvalidRunResult)
			}
		}
		sampleCounts[profileIndex]++
	}

	derivedProfileResults, derivedOutcome, err := deriveRunSummary(result, sampleCounts)
	if err != nil {
		return err
	}
	if !exactProfileRunResults(result.ProfileResults, derivedProfileResults) {
		return fmt.Errorf("%w: profile results were not derived in exact mode order", ErrInvalidRunResult)
	}
	if result.RunOutcome != derivedOutcome {
		return fmt.Errorf("%w: run outcome %q was not derived as %q", ErrInvalidRunResult, result.RunOutcome, derivedOutcome)
	}
	return nil
}

func repeatedObservedAuthority(
	sample SampleResult,
	processInstanceIDs map[string]struct{},
	attemptIDs map[string]struct{},
	challengeBindings map[string]struct{},
) bool {
	if sample.ProcessInstanceID == nil || sample.AttemptEvidence == nil {
		return true
	}
	processInstanceID := *sample.ProcessInstanceID
	attemptID := sample.AttemptEvidence.AttemptAuthority.AttemptID
	challengeBinding := ""
	if sample.AttemptEvidence.Challenge != nil {
		challengeBinding = sample.AttemptEvidence.Challenge.BindingSHA256
	}
	_, repeatedProcess := processInstanceIDs[processInstanceID]
	_, repeatedAttempt := attemptIDs[attemptID]
	_, repeatedChallenge := challengeBindings[challengeBinding]
	if repeatedProcess || repeatedAttempt || challengeBinding != "" && repeatedChallenge {
		return true
	}
	processInstanceIDs[processInstanceID] = struct{}{}
	attemptIDs[attemptID] = struct{}{}
	if challengeBinding != "" {
		challengeBindings[challengeBinding] = struct{}{}
	}
	return false
}

func deriveRunSummary(
	result RunResult,
	sampleCounts []int,
) ([]ProfileRunResult, RunOutcome, error) {
	if len(sampleCounts) != len(result.RuntimeAttestations) {
		return nil, "", fmt.Errorf("%w: sample counters differ from the profile registry", ErrInvalidRunResult)
	}

	switch result.OrchestrationOutcome {
	case OrchestrationHealthy:
		if result.OrchestrationFailure != nil {
			return nil, "", fmt.Errorf("%w: healthy orchestration carries failure", ErrInvalidRunResult)
		}
	case OrchestrationFailed:
		if result.OrchestrationFailure == nil || !validOrchestrationFailure(result.OrchestrationFailure.FailureCode) {
			return nil, "", fmt.Errorf("%w: failed orchestration lacks typed failure", ErrInvalidRunResult)
		}
	default:
		return nil, "", fmt.Errorf("%w: orchestration outcome is unknown", ErrInvalidRunResult)
	}

	infrastructureFailures := make([]int, len(result.RuntimeAttestations))
	for _, sample := range result.Samples {
		if sample.SampleOutcome != SampleInfrastructureFailed {
			continue
		}
		profileIndex := findAttestationIndex(result.RuntimeAttestations, sample.Identity.ProfileID)
		if profileIndex < 0 {
			return nil, "", fmt.Errorf("%w: infrastructure sample has no runtime attestation", ErrInvalidRunResult)
		}
		infrastructureFailures[profileIndex]++
	}

	profileResults := make([]ProfileRunResult, len(result.RuntimeAttestations))
	completedProfiles := 0
	profileInfrastructureFailed := false
	for index, attestation := range result.RuntimeAttestations {
		profileResult := ProfileRunResult{
			ProfileID:                    attestation.ProfileID,
			PrerequisiteOutcome:          attestation.PrerequisiteOutcome,
			ExpectedSamples:              identitiesPerProfile,
			ObservedSamples:              sampleCounts[index],
			SampleInfrastructureFailures: infrastructureFailures[index],
		}
		if attestation.PrerequisiteOutcome != PrerequisiteSatisfied {
			if sampleCounts[index] != 0 || infrastructureFailures[index] != 0 {
				return nil, "", fmt.Errorf("%w: unsatisfied profile %q carries samples", ErrInvalidRunResult, attestation.ProfileID)
			}
			profileResult.ProfileOutcome = ProfileNotExecuted
			profileResults[index] = profileResult
			continue
		}

		if result.OrchestrationOutcome == OrchestrationHealthy && sampleCounts[index] != identitiesPerProfile {
			return nil, "", fmt.Errorf("%w: healthy satisfied profile %q lacks its exact 3x5 samples", ErrInvalidRunResult, attestation.ProfileID)
		}
		// Profile completeness is derived from its own evidence ledger. A later
		// orchestration failure must not erase a profile that already completed.
		if sampleCounts[index] == identitiesPerProfile && infrastructureFailures[index] == 0 {
			profileResult.ProfileOutcome = ProfileCompleted
			completedProfiles++
		} else {
			profileResult.ProfileOutcome = ProfileInfrastructureFailed
			profileInfrastructureFailed = true
		}
		profileResults[index] = profileResult
	}

	if result.OrchestrationOutcome == OrchestrationFailed || profileInfrastructureFailed {
		return profileResults, RunInfrastructureFailed, nil
	}
	switch {
	case completedProfiles == len(profileResults):
		return profileResults, RunCompleted, nil
	case completedProfiles == 0:
		return profileResults, RunNotExecuted, nil
	default:
		return profileResults, RunPartial, nil
	}
}

func findAttestationIndex(attestations []RuntimeAttestation, profileID string) int {
	for index, attestation := range attestations {
		if attestation.ProfileID == profileID {
			return index
		}
	}
	return -1
}

func exactProfileRunResults(actual, expected []ProfileRunResult) bool {
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

func validOrchestrationFailure(code OrchestrationFailureCode) bool {
	return code == FailureRuntimeBootstrap || code == FailureOrchestratorDeadline ||
		code == FailureCollector || code == FailureContainmentCleanup
}

func findIdentityIndex(identities []SampleIdentity, expected SampleIdentity) int {
	for index, identity := range identities {
		if identity == expected {
			return index
		}
	}
	return -1
}

func findStringIndex(values []string, expected string) int {
	for index, value := range values {
		if value == expected {
			return index
		}
	}
	return -1
}

func (result RunResult) CanonicalJSON(contract Contract) ([]byte, error) {
	if err := result.Validate(contract); err != nil {
		return nil, err
	}
	return marshalCanonicalDocument(result)
}

func (result RunResult) SHA256(contract Contract) (string, error) {
	encoded, err := result.CanonicalJSON(contract)
	if err != nil {
		return "", err
	}
	return sha256Text(encoded), nil
}
