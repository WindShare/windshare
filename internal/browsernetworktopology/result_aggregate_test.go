package browsernetworktopology

import (
	"bytes"
	"errors"
	"testing"
)

func TestSampleResultRoundTripAndDerivedPolicyEvaluation(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	attestation := satisfiedAttestation(t, contract, "run-sample", string(ProfileScheduledPublicSTUN))
	identity := SampleIdentity{
		ProfileID: string(ProfileScheduledPublicSTUN), Browser: BrowserFirefox, SampleOrdinal: 3,
	}
	sample := observedSample(t, contract, attestation, identity)
	encoded, err := sample.CanonicalJSON(contract, attestation)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	parsed, err := ParseSampleResult(encoded, contract, attestation)
	if err != nil || parsed.Identity != identity {
		t.Fatalf("ParseSampleResult: parsed=%+v err=%v", parsed, err)
	}

	tampered := sample
	tampered.CandidatePolicyOutcome = CandidatePolicyMismatched
	tampered.RationaleCodes = []CandidateRationaleCode{RationaleProtocolForbidden}
	if err := tampered.Validate(contract, attestation); !errors.Is(err, ErrInvalidSampleResult) {
		t.Fatalf("forged evaluation error = %v", err)
	}
	tampered = sample
	tampered.AttestationSHA256 = fixtureManifestSHA256
	if err := tampered.Validate(contract, attestation); !errors.Is(err, ErrInvalidSampleResult) {
		t.Fatalf("attestation digest mismatch error = %v", err)
	}
	if _, err := ParseSampleResult(addRootMember(encoded, `"unknown":true`), contract, attestation); !errors.Is(err, ErrInvalidSampleResult) {
		t.Fatalf("unknown field error = %v", err)
	}
	unknownAttemptField := bytes.Replace(
		encoded,
		[]byte(`"externalFixture":{`),
		[]byte(`"externalFixture":{"unknown":true,`),
		1,
	)
	if _, err := ParseSampleResult(unknownAttemptField, contract, attestation); !errors.Is(err, ErrInvalidSampleResult) {
		t.Fatalf("unknown signed attempt field error = %v", err)
	}
	if _, err := ParseSampleResult(encoded[:len(encoded)-1], contract, attestation); !errors.Is(err, ErrNonCanonicalJSON) {
		t.Fatalf("noncanonical error = %v", err)
	}
}

func TestSampleAttemptEvidenceRequiresOneProfileBoundTwoEndedAttempt(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	identity := SampleIdentity{
		ProfileID: string(ProfileScheduledPublicSTUN), Browser: BrowserChromium, SampleOrdinal: 1,
	}
	attestation := satisfiedAttestation(t, contract, "run-attempt-evidence", identity.ProfileID)
	tests := []struct {
		name   string
		mutate func(*AttemptEvidence)
	}{
		{name: "wrong Pion authority", mutate: func(evidence *AttemptEvidence) {
			evidence.PionAuthority = PionAuthority("fabricated-remote-authority")
		}},
		{name: "pair presence mismatch", mutate: func(evidence *AttemptEvidence) {
			evidence.PionSelectedPair = PionSelectedPair{SelectedPair: SelectedPairAbsent}
		}},
		{name: "crossed peer candidate mismatch", mutate: func(evidence *AttemptEvidence) {
			candidate := CandidateHost
			evidence.PionSelectedPair.RemoteCandidateType = &candidate
		}},
		{name: "missing established challenge", mutate: func(evidence *AttemptEvidence) {
			evidence.Challenge = nil
		}},
		{name: "challenge not echoed", mutate: func(evidence *AttemptEvidence) {
			evidence.Challenge.BrowserEchoObserved = false
		}},
		{name: "missing Pion address family", mutate: func(evidence *AttemptEvidence) {
			evidence.PionSelectedPair.RemoteAddressFamily = nil
		}},
		{name: "unbounded attempt authority", mutate: func(evidence *AttemptEvidence) {
			evidence.AttemptAuthority.AttemptID = "short"
		}},
		{name: "crossed runtime fixture binding", mutate: func(evidence *AttemptEvidence) {
			evidence.ExternalFixture.RemotePeerBindingSHA256 = fixtureManifestSHA256
		}},
		{name: "tampered signed fixture", mutate: func(evidence *AttemptEvidence) {
			evidence.ExternalFixture.SignedAttestation.Signature = corruptFixtureSignature(
				evidence.ExternalFixture.SignedAttestation.Signature,
			)
		}},
		{name: "tampered terminal receipt", mutate: func(evidence *AttemptEvidence) {
			evidence.TerminalReceipt.Signature = corruptFixtureSignature(
				evidence.TerminalReceipt.Signature,
			)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sample := observedSample(t, contract, attestation, identity)
			test.mutate(sample.AttemptEvidence)
			if err := sample.Validate(contract, attestation); !errors.Is(err, ErrInvalidAttemptEvidence) {
				t.Fatalf("Validate error = %v", err)
			}
		})
	}

	restrictedIdentity := SampleIdentity{
		ProfileID: string(ProfileScheduledRestrictedUDP), Browser: BrowserFirefox, SampleOrdinal: 2,
	}
	restrictedAttestation := satisfiedAttestation(
		t,
		contract,
		"run-restricted-attempt-evidence",
		restrictedIdentity.ProfileID,
	)
	restricted := observedSample(t, contract, restrictedAttestation, restrictedIdentity)
	restricted.AttemptEvidence.Challenge = &ChallengeProof{
		BindingSHA256:         fixtureManifestSHA256,
		PionChallengeObserved: true,
		BrowserEchoObserved:   true,
	}
	if err := restricted.Validate(contract, restrictedAttestation); !errors.Is(err, ErrInvalidAttemptEvidence) {
		t.Fatalf("restricted challenge error = %v", err)
	}
}

func corruptFixtureSignature(signature string) string {
	replacement := byte('A')
	if signature[len(signature)-1] == replacement {
		replacement = 'B'
	}
	return signature[:len(signature)-1] + string(replacement)
}

func TestRunRejectsReusedObservedProcessAttemptAndChallengeAuthorities(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	tests := []struct {
		name   string
		mutate func(*RunResult)
	}{
		{name: "process-instance", mutate: func(run *RunResult) {
			value := *run.Samples[0].ProcessInstanceID
			run.Samples[1].ProcessInstanceID = &value
		}},
		{name: "attempt", mutate: func(run *RunResult) {
			run.Samples[1].AttemptEvidence.AttemptAuthority.AttemptID =
				run.Samples[0].AttemptEvidence.AttemptAuthority.AttemptID
		}},
		{name: "challenge", mutate: func(run *RunResult) {
			run.Samples[1].AttemptEvidence.Challenge.BindingSHA256 =
				run.Samples[0].AttemptEvidence.Challenge.BindingSHA256
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := buildRun(
				t,
				contract,
				ModeScheduled,
				"run-reused-"+test.name,
				[]PrerequisiteOutcome{
					PrerequisiteSatisfied,
					PrerequisiteSatisfied,
					PrerequisiteSatisfied,
				},
				OrchestrationHealthy,
			)
			test.mutate(&run)
			if err := run.Validate(contract); !errors.Is(err, ErrInvalidRunResult) {
				t.Fatalf("Validate error = %v", err)
			}
		})
	}
}

func TestSampleInfrastructureFailureCannotCarryAttemptProof(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	attestation := satisfiedAttestation(t, contract, "run-sample-infra", string(ProfileScheduledCoturn))
	identity := SampleIdentity{
		ProfileID: string(ProfileScheduledCoturn), Browser: BrowserWebKit, SampleOrdinal: 5,
	}
	sample := observedSample(t, contract, attestation, identity)
	processInstanceID := sample.ProcessInstanceID
	attemptEvidence := sample.AttemptEvidence
	sample.SampleOutcome = SampleInfrastructureFailed
	sample.ProcessInstanceID = nil
	sample.AttemptEvidence = nil
	sample.CandidatePolicyOutcome = CandidatePolicyNotEvaluated
	sample.RationaleCodes = []CandidateRationaleCode{}
	sample.Failure = &SampleFailure{FailureCode: FailureSampleEvidenceCollection}
	if err := sample.Validate(contract, attestation); err != nil {
		t.Fatalf("valid infrastructure-failed sample: %v", err)
	}

	for _, mutate := range []func(*SampleResult){
		func(value *SampleResult) { value.ProcessInstanceID = processInstanceID },
		func(value *SampleResult) { value.AttemptEvidence = attemptEvidence },
		func(value *SampleResult) { value.CandidatePolicyOutcome = CandidatePolicyMatched },
		func(value *SampleResult) {
			value.RationaleCodes = []CandidateRationaleCode{RationaleProtocolNotAllowed}
		},
		func(value *SampleResult) { value.Failure = nil },
		func(value *SampleResult) { value.Failure = &SampleFailure{FailureCode: "unknown"} },
	} {
		value := sample
		mutate(&value)
		if err := value.Validate(contract, attestation); !errors.Is(err, ErrInvalidSampleResult) {
			t.Fatalf("hostile infrastructure sample error = %v", err)
		}
	}
}

func TestRunOutcomeIsDerivedFromPrerequisitesAndObservedSubset(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	tests := []struct {
		name        string
		mode        ExecutionMode
		outcomes    []PrerequisiteOutcome
		wantOutcome RunOutcome
		wantSamples int
	}{
		{
			name: "scheduled completed", mode: ModeScheduled,
			outcomes:    []PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
			wantOutcome: RunCompleted, wantSamples: ScheduledIdentityCount,
		},
		{
			name:        "scheduled partial by whole profile",
			mode:        ModeScheduled,
			outcomes:    []PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteUnavailable, PrerequisiteFailed},
			wantOutcome: RunPartial, wantSamples: identitiesPerProfile,
		},
		{
			name:        "scheduled not executed",
			mode:        ModeScheduled,
			outcomes:    []PrerequisiteOutcome{PrerequisiteUnavailable, PrerequisiteInvalid, PrerequisiteFailed},
			wantOutcome: RunNotExecuted, wantSamples: 0,
		},
		{
			name: "manual completed", mode: ModeManual,
			outcomes:    []PrerequisiteOutcome{PrerequisiteSatisfied},
			wantOutcome: RunCompleted, wantSamples: ManualIdentityCount,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := buildRun(t, contract, test.mode, "run-"+string(test.mode), test.outcomes, OrchestrationHealthy)
			if run.RunOutcome != test.wantOutcome || len(run.Samples) != test.wantSamples {
				t.Fatalf("run outcome=%q samples=%d", run.RunOutcome, len(run.Samples))
			}
			encoded, err := run.CanonicalJSON(contract)
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			if _, err := ParseRunResult(encoded, contract); err != nil {
				t.Fatalf("ParseRunResult: %v", err)
			}
		})
	}
}

func TestProfileResultsAreExactOrderedAndDerived(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	run := buildRun(
		t, contract, ModeScheduled, "run-profile-results",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteUnavailable, PrerequisiteFailed},
		OrchestrationHealthy,
	)
	want := []ProfileRunResult{
		{
			ProfileID: string(ProfileScheduledPublicSTUN), PrerequisiteOutcome: PrerequisiteSatisfied,
			ExpectedSamples: identitiesPerProfile, ObservedSamples: identitiesPerProfile,
			SampleInfrastructureFailures: 0, ProfileOutcome: ProfileCompleted,
		},
		{
			ProfileID: string(ProfileScheduledRestrictedUDP), PrerequisiteOutcome: PrerequisiteUnavailable,
			ExpectedSamples: identitiesPerProfile, ObservedSamples: 0,
			SampleInfrastructureFailures: 0, ProfileOutcome: ProfileNotExecuted,
		},
		{
			ProfileID: string(ProfileScheduledCoturn), PrerequisiteOutcome: PrerequisiteFailed,
			ExpectedSamples: identitiesPerProfile, ObservedSamples: 0,
			SampleInfrastructureFailures: 0, ProfileOutcome: ProfileNotExecuted,
		},
	}
	if !exactProfileRunResults(run.ProfileResults, want) || run.RunOutcome != RunPartial {
		t.Fatalf("derived profile results = %+v, run outcome = %q", run.ProfileResults, run.RunOutcome)
	}

	tests := []struct {
		name   string
		mutate func(*RunResult)
	}{
		{name: "missing", mutate: func(value *RunResult) { value.ProfileResults = value.ProfileResults[:2] }},
		{name: "unordered", mutate: func(value *RunResult) {
			value.ProfileResults[0], value.ProfileResults[1] = value.ProfileResults[1], value.ProfileResults[0]
		}},
		{name: "profile ID", mutate: func(value *RunResult) { value.ProfileResults[0].ProfileID = "fabricated" }},
		{name: "prerequisite", mutate: func(value *RunResult) {
			value.ProfileResults[0].PrerequisiteOutcome = PrerequisiteUnavailable
		}},
		{name: "expected count", mutate: func(value *RunResult) { value.ProfileResults[0].ExpectedSamples-- }},
		{name: "observed count", mutate: func(value *RunResult) { value.ProfileResults[0].ObservedSamples-- }},
		{name: "infrastructure count", mutate: func(value *RunResult) {
			value.ProfileResults[0].SampleInfrastructureFailures++
		}},
		{name: "profile outcome", mutate: func(value *RunResult) {
			value.ProfileResults[0].ProfileOutcome = ProfileInfrastructureFailed
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := run
			mutated.ProfileResults = append([]ProfileRunResult(nil), run.ProfileResults...)
			test.mutate(&mutated)
			if err := mutated.Validate(contract); !errors.Is(err, ErrInvalidRunResult) {
				t.Fatalf("forged profile result error = %v", err)
			}
		})
	}
}

func TestRunRejectsMissingDuplicateUnexpectedAndUnsatisfiedSamples(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	completed := buildRun(
		t, contract, ModeScheduled, "run-hostile-samples",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationHealthy,
	)
	tests := []struct {
		name   string
		mutate func(*RunResult)
	}{
		{name: "missing sample", mutate: func(run *RunResult) { run.Samples = run.Samples[:len(run.Samples)-1] }},
		{name: "duplicate sample", mutate: func(run *RunResult) { run.Samples[1] = run.Samples[0] }},
		{name: "unexpected ordinal", mutate: func(run *RunResult) { run.Samples[0].Identity.SampleOrdinal = 6 }},
		{name: "unordered samples", mutate: func(run *RunResult) { run.Samples[0], run.Samples[1] = run.Samples[1], run.Samples[0] }},
		{name: "missing expected identity", mutate: func(run *RunResult) {
			run.ExpectedIdentities = run.ExpectedIdentities[:len(run.ExpectedIdentities)-1]
		}},
		{name: "extra expected identity", mutate: func(run *RunResult) {
			run.ExpectedIdentities = append(run.ExpectedIdentities, run.ExpectedIdentities[0])
		}},
		{name: "forged partial", mutate: func(run *RunResult) { run.RunOutcome = RunPartial }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := completed
			run.Samples = append([]SampleResult(nil), completed.Samples...)
			run.ExpectedIdentities = append([]SampleIdentity(nil), completed.ExpectedIdentities...)
			test.mutate(&run)
			if err := run.Validate(contract); !errors.Is(err, ErrInvalidRunResult) {
				t.Fatalf("Validate error = %v", err)
			}
		})
	}

	partial := buildRun(
		t, contract, ModeScheduled, "run-unsatisfied-sample",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteUnavailable, PrerequisiteUnavailable},
		OrchestrationHealthy,
	)
	restrictedIdentity := partial.ExpectedIdentities[identitiesPerProfile]
	fakeSatisfied := satisfiedAttestation(t, contract, partial.RunID, restrictedIdentity.ProfileID)
	partial.Samples = append(partial.Samples, observedSample(t, contract, fakeSatisfied, restrictedIdentity))
	if err := partial.Validate(contract); !errors.Is(err, ErrInvalidRunResult) {
		t.Fatalf("unsatisfied profile sample error = %v", err)
	}
}

func TestInfrastructureFailedRunMayRetainOnlyActuallyStartedSamples(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	run := buildRun(
		t, contract, ModeScheduled, "run-infrastructure",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationFailed,
	)
	run.Samples = run.Samples[:7]
	refreshRunSummary(t, &run)
	if err := run.Validate(contract); err != nil {
		t.Fatalf("infrastructure failed subset: %v", err)
	}
	if run.RunOutcome != RunInfrastructureFailed || len(run.ExpectedIdentities) != ScheduledIdentityCount || len(run.Samples) != 7 ||
		run.ProfileResults[0].ObservedSamples != 7 ||
		run.ProfileResults[0].ProfileOutcome != ProfileInfrastructureFailed ||
		run.ProfileResults[1].ProfileOutcome != ProfileInfrastructureFailed ||
		run.ProfileResults[2].ProfileOutcome != ProfileInfrastructureFailed {
		t.Fatal("infrastructure failure synthesized or discarded sample identities")
	}

	run.OrchestrationFailure = nil
	if err := run.Validate(contract); !errors.Is(err, ErrInvalidRunResult) {
		t.Fatalf("missing orchestration failure error = %v", err)
	}
}

func TestRunFreezesOrchestrationFailureVocabulary(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	validCodes := []OrchestrationFailureCode{
		FailureRuntimeBootstrap,
		FailureOrchestratorDeadline,
		FailureCollector,
		FailureContainmentCleanup,
	}
	for _, code := range validCodes {
		t.Run(string(code), func(t *testing.T) {
			run := buildRun(
				t, contract, ModeScheduled, "run-orchestration-failure-"+string(code),
				[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
				OrchestrationFailed,
			)
			run.OrchestrationFailure = &OrchestrationFailure{FailureCode: code}
			if err := run.Validate(contract); err != nil {
				t.Fatalf("valid orchestration failure %q: %v", code, err)
			}
		})
	}

	for _, hostile := range []OrchestrationFailureCode{
		"runner-setup-failed",
		"cleanup-failed",
		"containment-cleanup",
		"containment-cleanup-failed ",
	} {
		t.Run("reject-"+string(hostile), func(t *testing.T) {
			run := buildRun(
				t, contract, ModeScheduled, "run-hostile-orchestration-failure",
				[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
				OrchestrationFailed,
			)
			run.OrchestrationFailure = &OrchestrationFailure{FailureCode: hostile}
			if err := run.Validate(contract); !errors.Is(err, ErrInvalidRunResult) {
				t.Fatalf("hostile orchestration failure %q error = %v", hostile, err)
			}
		})
	}
}

func TestFailedOrchestrationPreservesCompletedProfileSubset(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	run := buildRun(
		t, contract, ModeScheduled, "run-infrastructure-after-profile",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationFailed,
	)
	run.Samples = run.Samples[:identitiesPerProfile+4]
	refreshRunSummary(t, &run)
	if err := run.Validate(contract); err != nil {
		t.Fatalf("infrastructure failed after completed profile: %v", err)
	}
	if run.RunOutcome != RunInfrastructureFailed ||
		run.ProfileResults[0].ProfileOutcome != ProfileCompleted ||
		run.ProfileResults[0].ObservedSamples != identitiesPerProfile ||
		run.ProfileResults[1].ProfileOutcome != ProfileInfrastructureFailed ||
		run.ProfileResults[1].ObservedSamples != 4 ||
		run.ProfileResults[2].ProfileOutcome != ProfileInfrastructureFailed ||
		run.ProfileResults[2].ObservedSamples != 0 {
		t.Fatalf("global orchestration failure erased the real completed subset: %+v", run.ProfileResults)
	}
}

func TestFailedOrchestrationKeepsUnsatisfiedProfilesNotExecuted(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	run := buildRun(
		t, contract, ModeScheduled, "run-infrastructure-mixed",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteUnavailable, PrerequisiteFailed},
		OrchestrationFailed,
	)
	run.Samples = run.Samples[:4]
	refreshRunSummary(t, &run)
	if err := run.Validate(contract); err != nil {
		t.Fatalf("mixed infrastructure run: %v", err)
	}
	if run.RunOutcome != RunInfrastructureFailed ||
		run.ProfileResults[0].ProfileOutcome != ProfileInfrastructureFailed ||
		run.ProfileResults[0].ObservedSamples != 4 ||
		run.ProfileResults[1].ProfileOutcome != ProfileNotExecuted ||
		run.ProfileResults[1].ObservedSamples != 0 ||
		run.ProfileResults[2].ProfileOutcome != ProfileNotExecuted ||
		run.ProfileResults[2].ObservedSamples != 0 {
		t.Fatalf("mixed infrastructure profile results = %+v", run.ProfileResults)
	}

	allUnsatisfied := buildRun(
		t, contract, ModeScheduled, "run-infrastructure-before-start",
		[]PrerequisiteOutcome{PrerequisiteUnavailable, PrerequisiteInvalid, PrerequisiteFailed},
		OrchestrationFailed,
	)
	if err := allUnsatisfied.Validate(contract); err != nil {
		t.Fatalf("failed orchestration before any profile start: %v", err)
	}
	if allUnsatisfied.RunOutcome != RunInfrastructureFailed {
		t.Fatalf("failed orchestration was hidden as %q", allUnsatisfied.RunOutcome)
	}
	for _, profileResult := range allUnsatisfied.ProfileResults {
		if profileResult.ProfileOutcome != ProfileNotExecuted || profileResult.ObservedSamples != 0 {
			t.Fatalf("unsatisfied profile was fabricated as executed: %+v", profileResult)
		}
	}
}

func TestSampleInfrastructureFailureDoesNotMasqueradeAsRunPartial(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	run := buildRun(
		t, contract, ModeManual, "run-sample-failure",
		[]PrerequisiteOutcome{PrerequisiteSatisfied}, OrchestrationHealthy,
	)
	sample := &run.Samples[0]
	sample.SampleOutcome = SampleInfrastructureFailed
	sample.ProcessInstanceID = nil
	sample.AttemptEvidence = nil
	sample.CandidatePolicyOutcome = CandidatePolicyNotEvaluated
	sample.RationaleCodes = []CandidateRationaleCode{}
	sample.Failure = &SampleFailure{FailureCode: FailureSampleRunner}
	refreshRunSummary(t, &run)
	if err := run.Validate(contract); err != nil {
		t.Fatalf("run with sample infrastructure result: %v", err)
	}
	if run.RunOutcome != RunInfrastructureFailed ||
		run.ProfileResults[0].ProfileOutcome != ProfileInfrastructureFailed ||
		run.ProfileResults[0].SampleInfrastructureFailures != 1 {
		t.Fatalf("sample infrastructure failure was not elevated: run=%q profile=%+v", run.RunOutcome, run.ProfileResults[0])
	}
}

func TestRunParserRejectsUnknownAndNoncanonicalJSON(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	run := buildRun(
		t, contract, ModeManual, "run-canonical",
		[]PrerequisiteOutcome{PrerequisiteUnavailable}, OrchestrationHealthy,
	)
	encoded, err := run.CanonicalJSON(contract)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if _, err := ParseRunResult(addRootMember(encoded, `"unknown":true`), contract); !errors.Is(err, ErrInvalidRunResult) {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := ParseRunResult(encoded[:len(encoded)-1], contract); !errors.Is(err, ErrNonCanonicalJSON) {
		t.Fatalf("noncanonical error = %v", err)
	}
}

func TestAggregateIsPureObservationalAndDoesNotSynthesizeSamples(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	scheduled := buildRun(
		t, contract, ModeScheduled, "run-scheduled-aggregate",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationHealthy,
	)
	manual := buildRun(
		t, contract, ModeManual, "run-manual-aggregate",
		[]PrerequisiteOutcome{PrerequisiteUnavailable}, OrchestrationHealthy,
	)
	runs := []RunResult{scheduled, manual}
	aggregate, err := BuildAggregate(contract, runs)
	if err != nil {
		t.Fatalf("BuildAggregate: %v", err)
	}
	if aggregate.Counts.ExpectedIdentities != TotalIdentityCount ||
		aggregate.Counts.ObservedSamples != ScheduledIdentityCount ||
		aggregate.Counts.Matched != ScheduledIdentityCount ||
		aggregate.EvidenceOutcome != EvidenceIncomplete {
		t.Fatalf("aggregate = %+v", aggregate)
	}
	encoded, err := aggregate.CanonicalJSON(contract, runs)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if _, err := ParseAggregate(encoded, contract, runs); err != nil {
		t.Fatalf("ParseAggregate: %v", err)
	}
	for _, forbidden := range []string{"pass", "passed", "success"} {
		if bytesContains(encoded, forbidden) {
			t.Fatalf("aggregate encoded observational work as %q", forbidden)
		}
	}
}

func TestAggregateAcceptsOneRealRunAndCanonicalizesTwoRunOrder(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	scheduled := buildRun(
		t, contract, ModeScheduled, "run-scheduled-single",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationHealthy,
	)
	manual := buildRun(
		t, contract, ModeManual, "run-manual-single",
		[]PrerequisiteOutcome{PrerequisiteSatisfied}, OrchestrationHealthy,
	)

	for _, run := range []RunResult{scheduled, manual} {
		aggregate, err := BuildAggregate(contract, []RunResult{run})
		if err != nil {
			t.Fatalf("single %s aggregate: %v", run.ExecutionMode, err)
		}
		if len(aggregate.Runs) != 1 || aggregate.Runs[0].ExecutionMode != run.ExecutionMode ||
			aggregate.Counts.ExpectedIdentities != TotalIdentityCount ||
			aggregate.Counts.ObservedSamples != len(run.Samples) ||
			aggregate.EvidenceOutcome != EvidenceIncomplete {
			t.Fatalf("single %s aggregate = %+v", run.ExecutionMode, aggregate)
		}
		encoded, encodeErr := aggregate.CanonicalJSON(contract, []RunResult{run})
		if encodeErr != nil {
			t.Fatalf("single %s canonical aggregate: %v", run.ExecutionMode, encodeErr)
		}
		if _, parseErr := ParseAggregate(encoded, contract, []RunResult{run}); parseErr != nil {
			t.Fatalf("single %s parse aggregate: %v", run.ExecutionMode, parseErr)
		}
	}
	failedScheduled := buildRun(
		t, contract, ModeScheduled, "run-scheduled-single-infrastructure",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationFailed,
	)
	failedAggregate, err := BuildAggregate(contract, []RunResult{failedScheduled})
	if err != nil || failedAggregate.EvidenceOutcome != EvidenceInfrastructureFailed {
		t.Fatalf("single infrastructure aggregate = %+v, err=%v", failedAggregate, err)
	}

	aggregate, err := BuildAggregate(contract, []RunResult{manual, scheduled})
	if err != nil {
		t.Fatalf("reverse-order aggregate: %v", err)
	}
	if len(aggregate.Runs) != 2 ||
		aggregate.Runs[0].ExecutionMode != ModeScheduled ||
		aggregate.Runs[1].ExecutionMode != ModeManual ||
		aggregate.EvidenceOutcome != EvidenceComplete {
		t.Fatalf("canonical aggregate order = %+v", aggregate)
	}
}

func TestAggregateSeparatesCandidateMismatchFromEvidenceCompleteness(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	scheduled := buildRun(
		t, contract, ModeScheduled, "run-scheduled-mismatch",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationHealthy,
	)
	manual := buildRun(
		t, contract, ModeManual, "run-manual-complete",
		[]PrerequisiteOutcome{PrerequisiteSatisfied}, OrchestrationHealthy,
	)
	attemptEvidence := scheduled.Samples[0].AttemptEvidence
	local := CandidateHost
	attemptEvidence.BrowserSelectedPair.LocalCandidateType = &local
	profile, _, _ := contract.Profile(scheduled.Samples[0].Identity.ProfileID)
	outcome, rationales, err := EvaluateCandidatePath(
		profile.CandidatePolicy,
		profile.ConnectivityExpectation,
		attemptEvidence.BrowserSelectedPair,
	)
	if err != nil {
		t.Fatalf("EvaluateCandidatePath: %v", err)
	}
	scheduled.Samples[0].CandidatePolicyOutcome = outcome
	scheduled.Samples[0].RationaleCodes = rationales
	if err := scheduled.Validate(contract); err != nil ||
		scheduled.RunOutcome != RunCompleted ||
		scheduled.ProfileResults[0].ProfileOutcome != ProfileCompleted {
		t.Fatalf("candidate mismatch changed completion: run=%q profile=%q err=%v", scheduled.RunOutcome, scheduled.ProfileResults[0].ProfileOutcome, err)
	}
	aggregate, err := BuildAggregate(contract, []RunResult{scheduled, manual})
	if err != nil {
		t.Fatalf("BuildAggregate: %v", err)
	}
	if aggregate.EvidenceOutcome != EvidenceComplete || aggregate.Counts.Mismatched != 1 ||
		aggregate.Counts.Matched != TotalIdentityCount-1 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
}

func TestAggregateFailsClosedForInfrastructureAndInvalidRunSets(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	scheduled := buildRun(
		t, contract, ModeScheduled, "run-scheduled-infra",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationHealthy,
	)
	manual := buildRun(
		t, contract, ModeManual, "run-manual-infra",
		[]PrerequisiteOutcome{PrerequisiteSatisfied}, OrchestrationHealthy,
	)
	manual.Samples[0].SampleOutcome = SampleInfrastructureFailed
	manual.Samples[0].ProcessInstanceID = nil
	manual.Samples[0].AttemptEvidence = nil
	manual.Samples[0].CandidatePolicyOutcome = CandidatePolicyNotEvaluated
	manual.Samples[0].RationaleCodes = []CandidateRationaleCode{}
	manual.Samples[0].Failure = &SampleFailure{FailureCode: FailureSampleDeadline}
	refreshRunSummary(t, &manual)
	aggregate, err := BuildAggregate(contract, []RunResult{scheduled, manual})
	if err != nil || aggregate.EvidenceOutcome != EvidenceInfrastructureFailed ||
		aggregate.Counts.SampleInfrastructureFailures != 1 {
		t.Fatalf("infrastructure aggregate = %+v, err=%v", aggregate, err)
	}

	secondScheduled := buildRun(
		t, contract, ModeScheduled, "run-second-scheduled",
		[]PrerequisiteOutcome{PrerequisiteSatisfied, PrerequisiteSatisfied, PrerequisiteSatisfied},
		OrchestrationHealthy,
	)
	duplicateIDManual := buildRun(
		t, contract, ModeManual, scheduled.RunID,
		[]PrerequisiteOutcome{PrerequisiteSatisfied}, OrchestrationHealthy,
	)
	for _, runs := range [][]RunResult{
		nil,
		{scheduled, secondScheduled},
		{scheduled, duplicateIDManual},
		{scheduled, manual, manual},
	} {
		if _, err := BuildAggregate(contract, runs); !errors.Is(err, ErrInvalidAggregate) {
			t.Fatalf("runs %d error = %v", len(runs), err)
		}
	}

	invalidScheduled := scheduled
	invalidScheduled.Samples = invalidScheduled.Samples[:len(invalidScheduled.Samples)-1]
	if _, err := BuildAggregate(contract, []RunResult{invalidScheduled, manual}); !errors.Is(err, ErrInvalidAggregate) {
		t.Fatalf("invalid run aggregate error = %v", err)
	}
	forgedProfileResult := scheduled
	forgedProfileResult.ProfileResults = append([]ProfileRunResult(nil), scheduled.ProfileResults...)
	forgedProfileResult.ProfileResults[0].ObservedSamples--
	if _, err := BuildAggregate(contract, []RunResult{forgedProfileResult}); !errors.Is(err, ErrInvalidAggregate) {
		t.Fatalf("forged profile result aggregate error = %v", err)
	}
}

func TestAggregateRejectsForgedCountsDigestsAndNoncanonicalJSON(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	runs := []RunResult{
		buildRun(
			t, contract, ModeScheduled, "run-scheduled-forge",
			[]PrerequisiteOutcome{PrerequisiteUnavailable, PrerequisiteUnavailable, PrerequisiteUnavailable},
			OrchestrationHealthy,
		),
		buildRun(
			t, contract, ModeManual, "run-manual-forge",
			[]PrerequisiteOutcome{PrerequisiteUnavailable}, OrchestrationHealthy,
		),
	}
	aggregate, err := BuildAggregate(contract, runs)
	if err != nil {
		t.Fatalf("BuildAggregate: %v", err)
	}
	forged := aggregate
	forged.Counts.ObservedSamples = 1
	if err := forged.Validate(contract, runs); !errors.Is(err, ErrInvalidAggregate) {
		t.Fatalf("forged count error = %v", err)
	}
	forged = aggregate
	forged.Runs = append([]AggregateRunReference(nil), aggregate.Runs...)
	forged.Runs[0].RunSHA256 = fixtureManifestSHA256
	if err := forged.Validate(contract, runs); !errors.Is(err, ErrInvalidAggregate) {
		t.Fatalf("forged digest error = %v", err)
	}
	encoded, err := aggregate.CanonicalJSON(contract, runs)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if _, err := ParseAggregate(addRootMember(encoded, `"unknown":true`), contract, runs); !errors.Is(err, ErrInvalidAggregate) {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := ParseAggregate(encoded[:len(encoded)-1], contract, runs); !errors.Is(err, ErrNonCanonicalJSON) {
		t.Fatalf("noncanonical error = %v", err)
	}
}

func bytesContains(encoded []byte, text string) bool {
	for index := 0; index+len(text) <= len(encoded); index++ {
		if string(encoded[index:index+len(text)]) == text {
			return true
		}
	}
	return false
}
