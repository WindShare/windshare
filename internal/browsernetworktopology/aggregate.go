package browsernetworktopology

import (
	"errors"
	"fmt"
	"reflect"
)

var ErrInvalidAggregate = errors.New("invalid browser network matrix aggregate")

type AggregateRunReference struct {
	ExecutionMode ExecutionMode `json:"executionMode"`
	RunID         string        `json:"runId"`
	RunSHA256     string        `json:"runSha256"`
	RunOutcome    RunOutcome    `json:"runOutcome"`
}

type AggregateCounts struct {
	ExpectedIdentities           int `json:"expectedIdentities"`
	ObservedSamples              int `json:"observedSamples"`
	Matched                      int `json:"matched"`
	Mismatched                   int `json:"mismatched"`
	NotEvaluated                 int `json:"notEvaluated"`
	SampleInfrastructureFailures int `json:"sampleInfrastructureFailures"`
}

type EvidenceOutcome string

const (
	EvidenceComplete             EvidenceOutcome = "complete"
	EvidenceIncomplete           EvidenceOutcome = "incomplete"
	EvidenceInfrastructureFailed EvidenceOutcome = "infrastructure-failed"
)

type Aggregate struct {
	SchemaVersion      string                  `json:"schemaVersion"`
	MatrixID           string                  `json:"matrixId"`
	ManifestSHA256     string                  `json:"manifestSha256"`
	ReportingSemantics string                  `json:"reportingSemantics"`
	Runs               []AggregateRunReference `json:"runs"`
	Counts             AggregateCounts         `json:"counts"`
	EvidenceOutcome    EvidenceOutcome         `json:"evidenceOutcome"`
}

// BuildAggregate is intentionally pure. A caller may ignore its observational
// outcome for PR gating, but cannot turn missing or malformed inputs into success.
func BuildAggregate(contract Contract, runs []RunResult) (Aggregate, error) {
	canonicalRuns, err := validateAndOrderAggregateRuns(contract, runs)
	if err != nil {
		return Aggregate{}, err
	}

	aggregate := Aggregate{
		SchemaVersion:      AggregateSchemaVersion,
		MatrixID:           contract.MatrixID(),
		ManifestSHA256:     contract.ManifestSHA256(),
		ReportingSemantics: contract.ReportingSemantics(),
		Runs:               make([]AggregateRunReference, len(canonicalRuns)),
		Counts: AggregateCounts{
			ExpectedIdentities: TotalIdentityCount,
		},
		EvidenceOutcome: EvidenceIncomplete,
	}
	allCompleted := len(canonicalRuns) == 2
	for index, run := range canonicalRuns {
		runDigest, err := run.SHA256(contract)
		if err != nil {
			return Aggregate{}, errors.Join(ErrInvalidAggregate, err)
		}
		aggregate.Runs[index] = AggregateRunReference{
			ExecutionMode: run.ExecutionMode,
			RunID:         run.RunID,
			RunSHA256:     runDigest,
			RunOutcome:    run.RunOutcome,
		}
		aggregate.Counts.ObservedSamples += len(run.Samples)
		for _, sample := range run.Samples {
			switch sample.CandidatePolicyOutcome {
			case CandidatePolicyMatched:
				aggregate.Counts.Matched++
			case CandidatePolicyMismatched:
				aggregate.Counts.Mismatched++
			case CandidatePolicyNotEvaluated:
				aggregate.Counts.NotEvaluated++
			}
			if sample.SampleOutcome == SampleInfrastructureFailed {
				aggregate.Counts.SampleInfrastructureFailures++
			}
		}
		if run.RunOutcome == RunInfrastructureFailed {
			aggregate.EvidenceOutcome = EvidenceInfrastructureFailed
		}
		if run.RunOutcome != RunCompleted {
			allCompleted = false
		}
	}
	if aggregate.Counts.SampleInfrastructureFailures > 0 {
		aggregate.EvidenceOutcome = EvidenceInfrastructureFailed
	} else if aggregate.EvidenceOutcome != EvidenceInfrastructureFailed && allCompleted {
		aggregate.EvidenceOutcome = EvidenceComplete
	}
	if aggregate.Counts.ObservedSamples != aggregate.Counts.Matched+
		aggregate.Counts.Mismatched+aggregate.Counts.NotEvaluated {
		return Aggregate{}, fmt.Errorf("%w: sample classifications do not partition observed identities", ErrInvalidAggregate)
	}
	return aggregate, nil
}

func validateAndOrderAggregateRuns(contract Contract, runs []RunResult) ([]RunResult, error) {
	if len(runs) < 1 || len(runs) > 2 {
		return nil, fmt.Errorf("%w: aggregate needs one or two real mode runs", ErrInvalidAggregate)
	}

	var scheduled RunResult
	var manual RunResult
	hasScheduled := false
	hasManual := false
	seenRunIDs := make(map[string]struct{}, len(runs))
	for index := range runs {
		run := runs[index]
		if err := run.Validate(contract); err != nil {
			return nil, errors.Join(ErrInvalidAggregate, err)
		}
		if _, duplicate := seenRunIDs[run.RunID]; duplicate {
			return nil, fmt.Errorf("%w: aggregate run identity %q repeats", ErrInvalidAggregate, run.RunID)
		}
		seenRunIDs[run.RunID] = struct{}{}
		switch run.ExecutionMode {
		case ModeScheduled:
			if hasScheduled {
				return nil, fmt.Errorf("%w: scheduled mode run repeats", ErrInvalidAggregate)
			}
			scheduled = run
			hasScheduled = true
		case ModeManual:
			if hasManual {
				return nil, fmt.Errorf("%w: manual mode run repeats", ErrInvalidAggregate)
			}
			manual = run
			hasManual = true
		default:
			return nil, fmt.Errorf("%w: aggregate run mode is unknown", ErrInvalidAggregate)
		}
	}

	// Canonical mode order makes the same real run set hash identically without
	// manufacturing a reference for a mode that did not execute.
	ordered := make([]RunResult, 0, len(runs))
	if hasScheduled {
		ordered = append(ordered, scheduled)
	}
	if hasManual {
		ordered = append(ordered, manual)
	}
	return ordered, nil
}

func ParseAggregate(encoded []byte, contract Contract, runs []RunResult) (Aggregate, error) {
	var aggregate Aggregate
	if err := decodeCanonicalDocument(encoded, "browser network matrix aggregate", &aggregate, ErrInvalidAggregate); err != nil {
		return Aggregate{}, err
	}
	if err := aggregate.Validate(contract, runs); err != nil {
		return Aggregate{}, err
	}
	return aggregate, nil
}

func (aggregate Aggregate) Validate(contract Contract, runs []RunResult) error {
	expected, err := BuildAggregate(contract, runs)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(aggregate, expected) {
		return fmt.Errorf("%w: aggregate was not purely derived from its run inputs", ErrInvalidAggregate)
	}
	return nil
}

func (aggregate Aggregate) CanonicalJSON(contract Contract, runs []RunResult) ([]byte, error) {
	if err := aggregate.Validate(contract, runs); err != nil {
		return nil, err
	}
	return marshalCanonicalDocument(aggregate)
}

func (aggregate Aggregate) SHA256(contract Contract, runs []RunResult) (string, error) {
	encoded, err := aggregate.CanonicalJSON(contract, runs)
	if err != nil {
		return "", err
	}
	return sha256Text(encoded), nil
}
