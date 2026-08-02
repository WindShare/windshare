package browsernetworktopology

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const updateBrowserNetworkVectorsEnvironment = "WINDSHARE_UPDATE_BROWSER_NETWORK_VECTORS"

func TestSharedScheduledVectorsMatchTypeScriptContract(t *testing.T) {
	contract, _, _ := loadFixtureContract(t)
	vectorRoot := filepath.Join("..", "..", "testdata", "browser-network-matrix", "vectors")

	const observedRunID = "shared-observed-sample-run"
	attestation := satisfiedAttestation(
		t,
		contract,
		observedRunID,
		string(ProfileScheduledPublicSTUN),
	)
	identity := SampleIdentity{
		ProfileID:     string(ProfileScheduledPublicSTUN),
		Browser:       BrowserChromium,
		SampleOrdinal: 1,
	}
	sample := observedSample(t, contract, attestation, identity)

	const incompleteRunID = "shared-scheduled-not-executed-run"
	run := buildRun(
		t,
		contract,
		ModeScheduled,
		incompleteRunID,
		[]PrerequisiteOutcome{
			PrerequisiteUnavailable,
			PrerequisiteUnavailable,
			PrerequisiteUnavailable,
		},
		OrchestrationHealthy,
	)
	aggregate, err := BuildAggregate(contract, []RunResult{run})
	if err != nil {
		t.Fatalf("BuildAggregate shared vector: %v", err)
	}

	documents := []struct {
		name    string
		encoded []byte
	}{
		{name: "public-observed.attestation.v3.json", encoded: mustCanonicalAttestation(t, attestation, contract)},
		{name: "public-observed.sample.v2.json", encoded: mustCanonicalSample(t, sample, contract, attestation)},
		{name: "scheduled-not-executed.run.v2.json", encoded: mustCanonicalRun(t, run, contract)},
		{name: "scheduled-incomplete.verdict.v2.json", encoded: mustCanonicalAggregate(t, aggregate, contract, run)},
	}
	if os.Getenv(updateBrowserNetworkVectorsEnvironment) == "1" {
		if err := os.MkdirAll(vectorRoot, 0o755); err != nil {
			t.Fatalf("create vector directory: %v", err)
		}
		for _, document := range documents {
			if err := os.WriteFile(filepath.Join(vectorRoot, document.name), document.encoded, 0o644); err != nil {
				t.Fatalf("write %s: %v", document.name, err)
			}
		}
	}

	for _, document := range documents {
		committed := mustReadFile(t, filepath.Join(vectorRoot, document.name))
		if !bytes.Equal(committed, document.encoded) {
			t.Fatalf("shared vector %s differs; regenerate with %s=1", document.name, updateBrowserNetworkVectorsEnvironment)
		}
	}

	parsedAttestation, err := ParseRuntimeAttestation(documents[0].encoded, contract)
	if err != nil {
		t.Fatalf("ParseRuntimeAttestation shared vector: %v", err)
	}
	parsedSample, err := ParseSampleResult(documents[1].encoded, contract, parsedAttestation)
	if err != nil || parsedSample.ProcessInstanceID == nil || parsedSample.AttemptEvidence == nil ||
		parsedSample.AttemptEvidence.PionAuthority != PionAuthorityExternalRemote ||
		parsedSample.AttemptEvidence.Challenge == nil {
		t.Fatalf("shared sample semantics differ: sample=%+v err=%v", parsedSample, err)
	}
	parsedRun, err := ParseRunResult(documents[2].encoded, contract)
	if err != nil || parsedRun.ExecutionMode != ModeScheduled || parsedRun.RunOutcome != RunNotExecuted ||
		len(parsedRun.Samples) != 0 || len(parsedRun.ProfileResults) != len(frozenProfileSpecs) {
		t.Fatalf("shared run semantics differ: run=%+v err=%v", parsedRun, err)
	}
	parsedAggregate, err := ParseAggregate(documents[3].encoded, contract, []RunResult{parsedRun})
	if err != nil || len(parsedAggregate.Runs) != 1 ||
		parsedAggregate.Runs[0].ExecutionMode != ModeScheduled ||
		parsedAggregate.Counts.ExpectedIdentities != TotalIdentityCount ||
		parsedAggregate.Counts.ObservedSamples != 0 ||
		parsedAggregate.EvidenceOutcome != EvidenceIncomplete {
		t.Fatalf("shared aggregate semantics differ: aggregate=%+v err=%v", parsedAggregate, err)
	}
}

func mustCanonicalAttestation(t *testing.T, value RuntimeAttestation, contract Contract) []byte {
	t.Helper()
	encoded, err := value.CanonicalJSON(contract)
	if err != nil {
		t.Fatalf("canonical runtime attestation: %v", err)
	}
	return encoded
}

func mustCanonicalSample(
	t *testing.T,
	value SampleResult,
	contract Contract,
	attestation RuntimeAttestation,
) []byte {
	t.Helper()
	encoded, err := value.CanonicalJSON(contract, attestation)
	if err != nil {
		t.Fatalf("canonical sample: %v", err)
	}
	return encoded
}

func mustCanonicalRun(t *testing.T, value RunResult, contract Contract) []byte {
	t.Helper()
	encoded, err := value.CanonicalJSON(contract)
	if err != nil {
		t.Fatalf("canonical run: %v", err)
	}
	return encoded
}

func mustCanonicalAggregate(
	t *testing.T,
	value Aggregate,
	contract Contract,
	run RunResult,
) []byte {
	t.Helper()
	encoded, err := value.CanonicalJSON(contract, []RunResult{run})
	if err != nil {
		t.Fatalf("canonical aggregate: %v", err)
	}
	return encoded
}
