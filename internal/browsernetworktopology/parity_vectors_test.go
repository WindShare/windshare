package browsernetworktopology

import (
	"bytes"
	"path/filepath"
	"testing"
)

const (
	sharedObservedSampleSHA256      = "7f308602aacf39a132715c46e4602155bbacf85835981c6e3d51df8a68745949"
	sharedObservedAttestationSHA256 = "eb2b2711938bb89566306d4c8f2b1b1c2a6531a4ad4fe42ec4e1959f7e46a094"
	sharedManualRunSHA256           = "09525e1b511ed0691688c04cb3b2b7a020d86d28e65de70077833e53aa503ce2"
	sharedManualAggregateSHA256     = "d8aa746fee51ca8d8d1fa5fff19d77146be204926c7bfdc2f5a319672797053d"
)

func TestSharedModeLocalVectorsMatchTypeScriptContract(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	vectorRoot := filepath.Join("..", "..", "testdata", "browser-network-matrix", "vectors")
	sampleJSON := mustReadFile(t, filepath.Join(vectorRoot, "public-observed.sample.v1.json"))
	if digest := sha256Text(sampleJSON); digest != sharedObservedSampleSHA256 {
		t.Fatalf("shared sample SHA256 = %q", digest)
	}
	const sampleRunID = "shared-observed-sample-run"
	attestation := satisfiedAttestation(
		t,
		contract,
		sampleRunID,
		string(ProfileScheduledPublicSTUN),
	)
	attestationDigest, digestErr := attestation.SHA256(contract)
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	if attestationDigest != sharedObservedAttestationSHA256 {
		t.Fatalf("shared attestation SHA256 = %q", attestationDigest)
	}
	sample, err := ParseSampleResult(sampleJSON, contract, attestation)
	if err != nil {
		t.Fatalf("ParseSampleResult shared vector: %v", err)
	}
	if sample.ProcessInstanceID == nil || sample.AttemptEvidence == nil ||
		sample.AttemptEvidence.PionAuthority != PionAuthorityExternalRemote ||
		sample.AttemptEvidence.Challenge == nil {
		t.Fatalf("shared sample semantics differ: %+v", sample)
	}
	canonicalSample, err := sample.CanonicalJSON(contract, attestation)
	if err != nil || !bytes.Equal(canonicalSample, sampleJSON) {
		t.Fatalf("shared sample canonical bytes differ: err=%v", err)
	}

	runJSON := mustReadFile(t, filepath.Join(vectorRoot, "manual-not-executed.run.v1.json"))
	if digest := sha256Text(runJSON); digest != sharedManualRunSHA256 {
		t.Fatalf("shared run SHA256 = %q", digest)
	}
	run, err := ParseRunResult(runJSON, contract)
	if err != nil {
		t.Fatalf("ParseRunResult shared vector: %v", err)
	}
	if run.ExecutionMode != ModeManual || run.RunOutcome != RunNotExecuted || len(run.Samples) != 0 ||
		len(run.ProfileResults) != 1 || run.ProfileResults[0].ProfileOutcome != ProfileNotExecuted {
		t.Fatalf("shared run semantics differ: %+v", run)
	}
	canonicalRun, err := run.CanonicalJSON(contract)
	if err != nil || !bytes.Equal(canonicalRun, runJSON) {
		t.Fatalf("shared run canonical bytes differ: err=%v", err)
	}

	aggregateJSON := mustReadFile(t, filepath.Join(vectorRoot, "manual-only.aggregate.v1.json"))
	if digest := sha256Text(aggregateJSON); digest != sharedManualAggregateSHA256 {
		t.Fatalf("shared aggregate SHA256 = %q", digest)
	}
	aggregate, err := ParseAggregate(aggregateJSON, contract, []RunResult{run})
	if err != nil {
		t.Fatalf("ParseAggregate shared vector: %v", err)
	}
	if len(aggregate.Runs) != 1 || aggregate.Runs[0].ExecutionMode != ModeManual ||
		aggregate.Counts.ExpectedIdentities != TotalIdentityCount || aggregate.Counts.ObservedSamples != 0 ||
		aggregate.EvidenceOutcome != EvidenceIncomplete {
		t.Fatalf("shared aggregate semantics differ: %+v", aggregate)
	}
	canonicalAggregate, err := aggregate.CanonicalJSON(contract, []RunResult{run})
	if err != nil || !bytes.Equal(canonicalAggregate, aggregateJSON) {
		t.Fatalf("shared aggregate canonical bytes differ: err=%v", err)
	}
}
