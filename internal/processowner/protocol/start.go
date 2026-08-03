package protocol

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	StartEvidenceSchemaVersion   = "windshare.process-owner-start-evidence/v1"
	StartDecisionSchemaVersion   = "windshare.process-owner-start-decision/v1"
	objectIdentityVolumeHexWidth = 16
	objectIdentityObjectHexWidth = 32
)

const (
	StartDecisionAccepted = "accepted"
	StartDecisionRejected = "rejected"
)

// ObjectIdentity names one immutable filesystem object without relying on a
// mutable path. Volume is a fixed-width 64-bit value and Object is a fixed-width
// 128-bit value, both lowercase hexadecimal. The wider object field preserves
// Windows FILE_ID_128 without weakening Linux device/inode identity.
type ObjectIdentity struct {
	Volume string `json:"volume"`
	Object string `json:"object"`
}

// StartEvidence is emitted only after the owner proves containment membership.
// The consumer must bind its decision to every field before the owner releases
// the suspended/gated target.
type StartEvidence struct {
	SchemaVersion string `json:"schema_version"`
	Identity
	Platform        string         `json:"platform"`
	ProcessID       int            `json:"process_id"`
	ProcessInstance string         `json:"process_instance"`
	Executable      ObjectIdentity `json:"executable"`
}

type StartDecision struct {
	SchemaVersion string `json:"schema_version"`
	Identity
	Platform        string         `json:"platform"`
	ProcessID       int            `json:"process_id"`
	ProcessInstance string         `json:"process_instance"`
	Executable      ObjectIdentity `json:"executable"`
	Outcome         string         `json:"outcome"`
	FailureCode     string         `json:"failure_code,omitempty"`
	FailureMessage  string         `json:"failure_message,omitempty"`
}

func NewStartDecision(evidence StartEvidence, outcome, failureCode, failureMessage string) StartDecision {
	return StartDecision{
		SchemaVersion:   StartDecisionSchemaVersion,
		Identity:        evidence.Identity,
		Platform:        evidence.Platform,
		ProcessID:       evidence.ProcessID,
		ProcessInstance: evidence.ProcessInstance,
		Executable:      evidence.Executable,
		Outcome:         outcome,
		FailureCode:     failureCode,
		FailureMessage:  failureMessage,
	}
}

// ValidateStartEvidenceForRequest rejects replay across commands before a
// consumer grants release authority. The process owner remains responsible for
// deriving these identities from retained OS handles rather than path strings.
func ValidateStartEvidenceForRequest(evidence StartEvidence, request Request) error {
	if err := ValidateRequest(request); err != nil {
		return fmt.Errorf("validate process-owner request: %w", err)
	}
	if err := ValidateStartEvidence(evidence); err != nil {
		return err
	}
	if evidence.Identity != request.Identity {
		return errors.New("process-owner start evidence identity does not match its request")
	}
	return nil
}

func ValidateStartEvidence(evidence StartEvidence) error {
	if evidence.SchemaVersion != StartEvidenceSchemaVersion {
		return errors.New("process-owner start-evidence schema is unsupported")
	}
	if err := ValidateIdentity(evidence.Identity); err != nil {
		return err
	}
	if evidence.Platform != PlatformWindowsJob && evidence.Platform != PlatformLinuxSubreaper {
		return errors.New("process-owner start evidence platform is unsupported")
	}
	if evidence.ProcessID < 1 {
		return errors.New("process-owner start evidence requires a positive PID")
	}
	if err := validateCanonicalUnsigned("process instance", evidence.ProcessInstance, false); err != nil {
		return err
	}
	if err := validateObjectIdentity(evidence.Executable); err != nil {
		return err
	}
	return validateDocumentSize(evidence, "process-owner start evidence")
}

func ValidateStartDecisionForEvidence(decision StartDecision, evidence StartEvidence) error {
	if err := ValidateStartEvidence(evidence); err != nil {
		return fmt.Errorf("validate bound process-owner start evidence: %w", err)
	}
	if decision.SchemaVersion != StartDecisionSchemaVersion {
		return errors.New("process-owner start-decision schema is unsupported")
	}
	if decision.Identity != evidence.Identity ||
		decision.Platform != evidence.Platform ||
		decision.ProcessID != evidence.ProcessID ||
		decision.ProcessInstance != evidence.ProcessInstance ||
		decision.Executable != evidence.Executable {
		return errors.New("process-owner start decision does not bind its exact evidence")
	}
	switch decision.Outcome {
	case StartDecisionAccepted:
		if decision.FailureCode != "" || decision.FailureMessage != "" {
			return errors.New("accepted process-owner start decision excludes failure details")
		}
	case StartDecisionRejected:
		if decision.FailureCode == "" || decision.FailureMessage == "" {
			return errors.New("rejected process-owner start decision requires failure details")
		}
		if err := validateRequiredDiagnostic("start rejection code", decision.FailureCode); err != nil {
			return err
		}
		if err := validateRequiredDiagnostic("start rejection message", decision.FailureMessage); err != nil {
			return err
		}
	default:
		return errors.New("process-owner start decision outcome is unsupported")
	}
	return validateDocumentSize(decision, "process-owner start decision")
}

func NewObjectIdentity64(volume, object uint64) ObjectIdentity {
	return ObjectIdentity{
		Volume: fmt.Sprintf("%016x", volume),
		Object: fmt.Sprintf("%032x", object),
	}
}

func NewObjectIdentity128(volume uint64, object [16]byte) ObjectIdentity {
	return ObjectIdentity{
		Volume: fmt.Sprintf("%016x", volume),
		Object: hex.EncodeToString(object[:]),
	}
}

func validateObjectIdentity(identity ObjectIdentity) error {
	if len(identity.Volume) != objectIdentityVolumeHexWidth || len(identity.Object) != objectIdentityObjectHexWidth {
		return errors.New("executable object identity has non-canonical widths")
	}
	for _, value := range []string{identity.Volume, identity.Object} {
		for index := range len(value) {
			character := value[index]
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return errors.New("executable object identity must be lowercase hexadecimal")
			}
		}
	}
	if identity.Object == strings.Repeat("0", len(identity.Object)) {
		return errors.New("executable object identity is unavailable")
	}
	return nil
}

func validateCanonicalUnsigned(label, value string, allowZero bool) error {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(parsed, 10) != value {
		return fmt.Errorf("%s must be a canonical unsigned decimal", label)
	}
	if !allowZero && parsed == 0 {
		return fmt.Errorf("%s must be positive", label)
	}
	return nil
}
