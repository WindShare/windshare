package protocol

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	SettlementSchemaVersion = "windshare.process-owner-settlement/v3"
	MaximumDiagnosticBytes  = 512
)

const (
	CleanupCompleted = "completed"
	CleanupFailed    = "failed"

	TreeProvenEmpty = "proven_empty"
	TreeNonempty    = "nonempty"
	TreeUnknown     = "unknown"

	TerminationNatural              = "natural"
	TerminationDeadline             = "deadline"
	TerminationStop                 = "stop"
	TerminationParentLost           = "parent_lost"
	TerminationInitializationFailed = "initialization_failed"
	TerminationStartRejected        = "start_rejected"
	TerminationOwnerFailure         = "owner_failure"

	TargetExited               = "exited"
	TargetSignaled             = "signaled"
	TargetSpawnFailed          = "spawn_failed"
	TargetNotStarted           = "not_started"
	TargetTerminalEvidenceLost = "terminal_evidence_lost"
	TargetStartEvidenceLost    = "start_evidence_lost"

	RootActive               = "active"
	RootExited               = "exited"
	RootSignaled             = "signaled"
	RootTerminalEvidenceLost = "terminal_evidence_lost"

	InputNotRequested = "not_requested"
	InputDelivered    = "delivered"
	InputFailed       = "failed"
	InputNotStarted   = "not_started"
	InputEvidenceLost = "evidence_lost"

	PlatformWindowsJob     = "windows_job"
	PlatformLinuxSubreaper = "linux_subreaper"
)

type TargetEvidence struct {
	Outcome        string `json:"outcome"`
	ExitCode       *int64 `json:"exit_code,omitempty"`
	Signal         string `json:"signal,omitempty"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
}

type InputEvidence struct {
	Outcome        string `json:"outcome"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
}

type CleanupEvidence struct {
	Outcome        string `json:"outcome"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
}

type FailureEvidence struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RootEvidence struct {
	PID      int    `json:"pid"`
	State    string `json:"state"`
	ExitCode *int64 `json:"exit_code,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

// PlatformEvidence carries bounded diagnostic facts. TreeState remains the
// platform-neutral verdict so callers never infer cleanup from backend counters.
type PlatformEvidence struct {
	Kind                       string        `json:"kind"`
	OwnerPID                   int           `json:"owner_pid,omitempty"`
	Root                       *RootEvidence `json:"root,omitempty"`
	RootStartTimeTicks         string        `json:"root_start_time_ticks,omitempty"`
	ActiveProcessCount         *uint32       `json:"active_process_count,omitempty"`
	InventoryScans             int           `json:"inventory_scans,omitempty"`
	MaximumObservedDescendants int           `json:"maximum_observed_descendants,omitempty"`
	QuietInventoryCount        int           `json:"quiet_inventory_count,omitempty"`
}

type Settlement struct {
	SchemaVersion string `json:"schema_version"`
	Identity
	TerminationReason string           `json:"termination_reason"`
	Target            TargetEvidence   `json:"target"`
	Input             InputEvidence    `json:"input"`
	TreeState         string           `json:"tree_state"`
	Cleanup           CleanupEvidence  `json:"cleanup"`
	OwnerFailure      *FailureEvidence `json:"owner_failure,omitempty"`
	Platform          PlatformEvidence `json:"platform"`
}

func ValidateSettlement(settlement Settlement) error {
	if settlement.SchemaVersion != SettlementSchemaVersion {
		return errors.New("process-owner settlement schema is unsupported")
	}
	if err := ValidateIdentity(settlement.Identity); err != nil {
		return err
	}
	if err := validateTargetEvidence(settlement.Target); err != nil {
		return err
	}
	switch settlement.TerminationReason {
	case TerminationNatural, TerminationDeadline, TerminationStop, TerminationParentLost,
		TerminationInitializationFailed, TerminationStartRejected, TerminationOwnerFailure:
	default:
		return errors.New("process-owner settlement termination reason is unsupported")
	}
	if err := validateSettlementState(settlement); err != nil {
		return err
	}
	switch settlement.Input.Outcome {
	case InputNotRequested, InputDelivered, InputNotStarted:
		if settlement.Input.FailureCode != "" || settlement.Input.FailureMessage != "" {
			return errors.New("successful input evidence excludes failure details")
		}
	case InputFailed, InputEvidenceLost:
		if settlement.Input.FailureCode == "" || settlement.Input.FailureMessage == "" {
			return errors.New("failed or lost input evidence requires failure details")
		}
	default:
		return errors.New("process-owner settlement input outcome is unsupported")
	}
	if err := validateOptionalDiagnostic("target failure code", settlement.Target.FailureCode); err != nil {
		return err
	}
	if err := validateOptionalDiagnostic("target failure message", settlement.Target.FailureMessage); err != nil {
		return err
	}
	if err := validateOptionalDiagnostic("input failure code", settlement.Input.FailureCode); err != nil {
		return err
	}
	if err := validateOptionalDiagnostic("input failure message", settlement.Input.FailureMessage); err != nil {
		return err
	}
	if err := validateOptionalDiagnostic("cleanup failure code", settlement.Cleanup.FailureCode); err != nil {
		return err
	}
	if err := validateOptionalDiagnostic("cleanup failure message", settlement.Cleanup.FailureMessage); err != nil {
		return err
	}
	if settlement.OwnerFailure != nil {
		if settlement.OwnerFailure.Code == "" || settlement.OwnerFailure.Message == "" {
			return errors.New("owner failure requires bounded code and message evidence")
		}
		if err := validateRequiredDiagnostic("owner failure code", settlement.OwnerFailure.Code); err != nil {
			return err
		}
		if err := validateRequiredDiagnostic("owner failure message", settlement.OwnerFailure.Message); err != nil {
			return err
		}
	}
	if settlement.TerminationReason == TerminationOwnerFailure && settlement.OwnerFailure == nil {
		return errors.New("owner-triggered termination requires owner failure evidence")
	}
	if (settlement.Target.Outcome == TargetTerminalEvidenceLost ||
		settlement.Target.Outcome == TargetStartEvidenceLost ||
		settlement.Input.Outcome == InputEvidenceLost) && settlement.OwnerFailure == nil {
		return errors.New("lost target or input evidence requires owner failure evidence")
	}
	if settlement.Platform.Root != nil && settlement.Platform.Root.State == RootTerminalEvidenceLost && settlement.OwnerFailure == nil {
		return errors.New("lost root terminal evidence requires owner failure evidence")
	}
	switch settlement.TreeState {
	case TreeProvenEmpty:
	case TreeNonempty, TreeUnknown:
	default:
		return errors.New("process-owner tree state is unsupported")
	}
	switch settlement.Cleanup.Outcome {
	case CleanupCompleted:
		if settlement.TreeState != TreeProvenEmpty {
			return errors.New("completed cleanup requires a proven-empty tree")
		}
		if settlement.Cleanup.FailureCode != "" || settlement.Cleanup.FailureMessage != "" {
			return errors.New("completed cleanup excludes failure evidence")
		}
	case CleanupFailed:
		if settlement.Cleanup.FailureCode == "" || settlement.Cleanup.FailureMessage == "" {
			return errors.New("failed cleanup requires bounded failure evidence")
		}
		if settlement.OwnerFailure == nil {
			return errors.New("failed cleanup requires orthogonal owner failure evidence")
		}
	default:
		return errors.New("process-owner cleanup outcome is unsupported")
	}
	if err := validatePlatformEvidence(settlement); err != nil {
		return err
	}
	return validateDocumentSize(settlement, "process-owner settlement")
}

// ValidateSettlementForRequest binds evidence that cannot be interpreted from
// the settlement alone. In particular, input outcomes are meaningful only
// relative to the request's declared private byte stream.
func ValidateSettlementForRequest(settlement Settlement, request Request) error {
	if err := ValidateRequest(request); err != nil {
		return fmt.Errorf("validate process-owner request: %w", err)
	}
	if err := ValidateSettlement(settlement); err != nil {
		return err
	}
	if settlement.Identity != request.Identity {
		return errors.New("process-owner settlement identity does not match its request")
	}
	if request.Command.Stdin == nil {
		if settlement.Input.Outcome != InputNotRequested {
			return errors.New("settlement input evidence contradicts an input-free request")
		}
		return nil
	}
	started, known := targetStarted(settlement.Target.Outcome)
	if known && !started {
		if settlement.Input.Outcome != InputNotStarted {
			return errors.New("known-unstarted target input requires not-started evidence")
		}
		return nil
	}
	if known {
		if settlement.Input.Outcome != InputDelivered && settlement.Input.Outcome != InputFailed &&
			settlement.Input.Outcome != InputEvidenceLost {
			return errors.New("known-started target input requires delivered, failed, or evidence-lost evidence")
		}
		return nil
	}
	if settlement.Input.Outcome != InputNotStarted && settlement.Input.Outcome != InputEvidenceLost {
		return errors.New("unknown target start requires not-started or evidence-lost input evidence")
	}
	return nil
}

func validateTargetEvidence(target TargetEvidence) error {
	switch target.Outcome {
	case TargetExited:
		if target.ExitCode == nil || target.Signal != "" || target.FailureCode != "" || target.FailureMessage != "" {
			return errors.New("exited target evidence is inconsistent")
		}
	case TargetSignaled:
		if target.ExitCode != nil || target.Signal == "" || target.FailureCode != "" || target.FailureMessage != "" {
			return errors.New("signaled target evidence is inconsistent")
		}
	case TargetSpawnFailed, TargetNotStarted, TargetTerminalEvidenceLost, TargetStartEvidenceLost:
		if target.ExitCode != nil || target.Signal != "" || target.FailureCode == "" || target.FailureMessage == "" {
			return errors.New("failed or lost target evidence is inconsistent")
		}
	default:
		return errors.New("process-owner target outcome is unsupported")
	}
	if err := validateOptionalDiagnostic("target signal", target.Signal); err != nil {
		return err
	}
	return nil
}

func validatePlatformEvidence(settlement Settlement) error {
	platform := settlement.Platform
	if platform.OwnerPID < 1 {
		return errors.New("process-owner platform evidence requires a positive owner PID")
	}
	if platform.Root != nil {
		if err := validateRootEvidence(*platform.Root); err != nil {
			return err
		}
	}
	if err := validateTargetRootConsistency(settlement.Target, platform.Root); err != nil {
		return err
	}
	if platform.InventoryScans < 0 || platform.MaximumObservedDescendants < 0 || platform.QuietInventoryCount < 0 {
		return errors.New("process-owner platform counters cannot be negative")
	}
	switch settlement.TreeState {
	case TreeProvenEmpty:
		if platform.ActiveProcessCount == nil || *platform.ActiveProcessCount != 0 {
			return errors.New("proven-empty tree evidence requires active_process_count zero")
		}
		if platform.Root != nil && platform.Root.State == RootActive {
			return errors.New("proven-empty tree excludes an active root")
		}
	case TreeNonempty:
		if platform.ActiveProcessCount == nil || *platform.ActiveProcessCount == 0 {
			return errors.New("nonempty tree evidence requires a positive active_process_count")
		}
	case TreeUnknown:
		if platform.ActiveProcessCount != nil {
			return errors.New("unknown tree evidence excludes an asserted active_process_count")
		}
	}
	switch platform.Kind {
	case PlatformWindowsJob:
		if platform.RootStartTimeTicks != "" || platform.InventoryScans != 0 ||
			platform.MaximumObservedDescendants != 0 || platform.QuietInventoryCount != 0 {
			return errors.New("windows Job evidence contains Linux-only fields")
		}
		if settlement.Target.Outcome == TargetSignaled || (platform.Root != nil && platform.Root.State == RootSignaled) {
			return errors.New("windows Job evidence cannot report POSIX signal termination")
		}
		if platform.Root != nil && settlement.Target.Outcome == TargetSpawnFailed {
			return errors.New("windows root creation contradicts target spawn-failure evidence")
		}
		if settlement.Target.ExitCode != nil && (*settlement.Target.ExitCode < 0 || *settlement.Target.ExitCode > int64(^uint32(0))) {
			return errors.New("windows target exit code is outside the DWORD range")
		}
		if platform.Root != nil && platform.Root.ExitCode != nil &&
			(*platform.Root.ExitCode < 0 || *platform.Root.ExitCode > int64(^uint32(0))) {
			return errors.New("windows root exit code is outside the DWORD range")
		}
	case PlatformLinuxSubreaper:
		if platform.Root != nil {
			if platform.RootStartTimeTicks != "" || settlement.TreeState != TreeUnknown {
				ticks, err := strconv.ParseUint(platform.RootStartTimeTicks, 10, 64)
				if err != nil || ticks == 0 || strconv.FormatUint(ticks, 10) != platform.RootStartTimeTicks {
					return errors.New("created Linux root requires canonical positive start-time ticks")
				}
			}
		} else if platform.RootStartTimeTicks != "" {
			return errors.New("linux evidence without a root excludes root start-time ticks")
		}
		if settlement.TreeState == TreeProvenEmpty && platform.Root != nil {
			const minimumQuietInventoryProof = 2
			if platform.QuietInventoryCount < minimumQuietInventoryProof ||
				platform.InventoryScans < platform.QuietInventoryCount {
				return errors.New("proven-empty Linux tree requires repeated quiet inventory evidence")
			}
		}
	default:
		return errors.New("process-owner settlement platform kind is unsupported")
	}
	return nil
}

func validateRootEvidence(root RootEvidence) error {
	if root.PID < 1 {
		return errors.New("created root evidence requires a positive PID")
	}
	switch root.State {
	case RootActive, RootTerminalEvidenceLost:
		if root.ExitCode != nil || root.Signal != "" {
			return errors.New("active or evidence-lost root excludes exact terminal evidence")
		}
	case RootExited:
		if root.ExitCode == nil || root.Signal != "" {
			return errors.New("exited root requires an exact exit code")
		}
	case RootSignaled:
		if root.ExitCode != nil || root.Signal == "" {
			return errors.New("signaled root requires an exact signal")
		}
	default:
		return errors.New("process-owner root state is unsupported")
	}
	if err := validateOptionalDiagnostic("root signal", root.Signal); err != nil {
		return err
	}
	return nil
}

func validateTargetRootConsistency(target TargetEvidence, root *RootEvidence) error {
	switch target.Outcome {
	case TargetExited:
		if root == nil || root.State != RootExited || root.ExitCode == nil || target.ExitCode == nil ||
			*root.ExitCode != *target.ExitCode {
			return errors.New("exited target requires matching root exit evidence")
		}
	case TargetSignaled:
		if root == nil || root.State != RootSignaled || root.Signal != target.Signal {
			return errors.New("signaled target requires matching root signal evidence")
		}
	case TargetTerminalEvidenceLost:
		if root != nil && root.State != RootTerminalEvidenceLost {
			return errors.New("lost target terminal evidence contradicts exact root terminal evidence")
		}
	}
	return nil
}

func validateSettlementState(settlement Settlement) error {
	outcome := settlement.Target.Outcome
	switch settlement.TerminationReason {
	case TerminationNatural:
		if outcome != TargetExited && outcome != TargetSignaled {
			return errors.New("natural termination requires exact target terminal evidence")
		}
	case TerminationDeadline, TerminationStop, TerminationParentLost:
		if outcome != TargetExited && outcome != TargetSignaled && outcome != TargetSpawnFailed && outcome != TargetNotStarted &&
			outcome != TargetTerminalEvidenceLost && outcome != TargetStartEvidenceLost {
			return errors.New("controlled termination target evidence is inconsistent")
		}
	case TerminationInitializationFailed:
		if outcome != TargetSpawnFailed && outcome != TargetNotStarted && outcome != TargetStartEvidenceLost {
			return errors.New("initialization failure target evidence is inconsistent")
		}
	case TerminationStartRejected:
		if outcome != TargetNotStarted {
			return errors.New("start rejection requires target-not-started evidence")
		}
		if settlement.OwnerFailure != nil {
			return errors.New("start rejection excludes owner-failure evidence")
		}
	case TerminationOwnerFailure:
		// Owner failure is the primary trigger; every exact or lost target state
		// remains independently reportable.
	}
	started, known := targetStarted(outcome)
	if known && !started && (settlement.Input.Outcome == InputDelivered || settlement.Input.Outcome == InputFailed ||
		settlement.Input.Outcome == InputEvidenceLost) {
		return errors.New("known-unstarted target cannot report attempted input delivery")
	}
	return nil
}

func targetStarted(outcome string) (started bool, known bool) {
	switch outcome {
	case TargetExited, TargetSignaled, TargetTerminalEvidenceLost:
		return true, true
	case TargetSpawnFailed, TargetNotStarted:
		return false, true
	case TargetStartEvidenceLost:
		return false, false
	default:
		return false, false
	}
}

func validateOptionalDiagnostic(label, value string) error {
	if value == "" {
		return nil
	}
	return validateRequiredDiagnostic(label, value)
}

func validateRequiredDiagnostic(label, value string) error {
	if value == "" || len(value) > MaximumDiagnosticBytes || strings.IndexByte(value, 0) >= 0 ||
		!utf8.ValidString(value) || !norm.NFC.IsNormalString(value) {
		return fmt.Errorf("%s must be non-empty NFC text without NUL containing at most %d UTF-8 bytes", label, MaximumDiagnosticBytes)
	}
	return nil
}
