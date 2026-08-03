// Package protocol defines the platform-neutral contract between process-owner
// clients and the external process-tree supervisor.
package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
	"golang.org/x/text/unicode/norm"
)

const (
	RequestSchemaVersion       = "windshare.process-owner-request/v2"
	ControlSchemaVersion       = "windshare.process-owner-control/v1"
	SettlementSchemaVersion    = "windshare.process-owner-settlement/v3"
	StartEvidenceSchemaVersion = "windshare.process-owner-start-evidence/v1"
	StartDecisionSchemaVersion = "windshare.process-owner-start-decision/v1"
	EventSchemaVersion         = testrun.EventSchemaVersion
	SelfCheckSchemaVersion     = "windshare.process-owner-self-check/v1"

	MaximumDocumentBytes           = testrun.MaximumEventDocumentBytes
	MaximumStdinBytes              = 1 << 20
	MaximumDiagnosticBytes         = 512
	MaximumDeadlineMilliseconds    = 3_600_000
	MaximumTerminationMilliseconds = 60_000
	objectIdentityVolumeHexWidth   = 16
	objectIdentityObjectHexWidth   = 32
)

const (
	ControlReasonStop       = "stop"
	ControlReasonParentLost = "parent_lost"
	ControlReasonDeadline   = "deadline"

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

	StartDecisionAccepted = "accepted"
	StartDecisionRejected = "rejected"
)

// Identity remains an alias at the process-owner boundary because test-run
// correlation does not grant process execution authority.
type Identity = testrun.Identity

type EnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Stdin declares the exact byte count delivered over the platform's private raw
// input channel. Keeping bytes off the JSON channel permits prompt zeroing and
// prevents command input from entering structured lifecycle logs.
type Stdin struct {
	ByteLength int64 `json:"byte_length"`
}

type Command struct {
	Executable       string             `json:"executable"`
	Arguments        []string           `json:"arguments"`
	WorkingDirectory string             `json:"working_directory"`
	Environment      []EnvironmentEntry `json:"environment"`
	Stdin            *Stdin             `json:"stdin"`
}

type Request struct {
	SchemaVersion string `json:"schema_version"`
	Identity
	Command                      Command `json:"command"`
	DeadlineMilliseconds         int64   `json:"deadline_milliseconds"`
	TerminationGraceMilliseconds int64   `json:"termination_grace_milliseconds"`
}

type Control struct {
	SchemaVersion string `json:"schema_version"`
	Identity
	Reason string `json:"reason"`
}

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

// Event is transported on a private inherited channel. The semantic contract
// lives in testrun; processowner only supplies containment and wire transport.
type Event = testrun.Event

func NewRequest(identity Identity, command Command, deadlineMilliseconds, terminationGraceMilliseconds int64) Request {
	return Request{
		SchemaVersion: RequestSchemaVersion, Identity: identity, Command: command,
		DeadlineMilliseconds:         deadlineMilliseconds,
		TerminationGraceMilliseconds: terminationGraceMilliseconds,
	}
}

func ValidateRequest(request Request) error {
	if request.SchemaVersion != RequestSchemaVersion {
		return errors.New("process-owner request schema is unsupported")
	}
	if err := ValidateIdentity(request.Identity); err != nil {
		return err
	}
	if request.DeadlineMilliseconds < 1 || request.DeadlineMilliseconds > MaximumDeadlineMilliseconds {
		return fmt.Errorf("deadline_milliseconds must be in [1, %d]", MaximumDeadlineMilliseconds)
	}
	if request.TerminationGraceMilliseconds < 1 || request.TerminationGraceMilliseconds > MaximumTerminationMilliseconds {
		return fmt.Errorf("termination_grace_milliseconds must be in [1, %d]", MaximumTerminationMilliseconds)
	}
	if err := ValidateCommand(request.Command); err != nil {
		return err
	}
	return validateDocumentSize(request, "process-owner request")
}

func ValidateControl(control Control, identity Identity) error {
	if control.SchemaVersion != ControlSchemaVersion {
		return errors.New("process-owner control schema is unsupported")
	}
	if control.Identity != identity {
		return errors.New("process-owner control identity does not match its request")
	}
	if control.Reason != ControlReasonStop && control.Reason != ControlReasonParentLost &&
		control.Reason != ControlReasonDeadline {
		return errors.New("process-owner control reason is unsupported")
	}
	return nil
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

func ValidateIdentity(identity Identity) error {
	return testrun.ValidateIdentity(identity)
}

func ValidateCommand(command Command) error {
	if err := validateNFCText("owned executable", command.Executable, false); err != nil {
		return err
	}
	if !filepath.IsAbs(command.Executable) || filepath.Clean(command.Executable) != command.Executable {
		return errors.New("owned executable must be an absolute canonical path")
	}
	if strings.IndexByte(command.Executable, 0) >= 0 {
		return errors.New("owned executable contains NUL")
	}
	if err := validateNFCText("owned working directory", command.WorkingDirectory, false); err != nil {
		return err
	}
	if !filepath.IsAbs(command.WorkingDirectory) || filepath.Clean(command.WorkingDirectory) != command.WorkingDirectory {
		return errors.New("owned working directory must be an absolute canonical path")
	}
	if command.Arguments == nil {
		return errors.New("owned command arguments must be an array")
	}
	for _, argument := range command.Arguments {
		if err := validateNFCText("owned command argument", argument, true); err != nil {
			return err
		}
	}
	if command.Environment == nil {
		return errors.New("owned command environment must be an array")
	}
	if err := validateEnvironment(command.Environment); err != nil {
		return err
	}
	if command.Stdin != nil && (command.Stdin.ByteLength < 1 || command.Stdin.ByteLength > MaximumStdinBytes) {
		return fmt.Errorf("owned command stdin byte_length must be in [1, %d]", MaximumStdinBytes)
	}
	return nil
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

func ValidateEvent(event Event) error {
	return testrun.ValidateEvent(event)
}

func validateEnvironment(environment []EnvironmentEntry) error {
	for index, entry := range environment {
		if err := validateNFCText("environment name", entry.Name, false); err != nil {
			return err
		}
		if err := validateNFCText("environment value", entry.Value, true); err != nil {
			return err
		}
		if entry.Name == "" || strings.ContainsAny(entry.Name, "=\x00") {
			return errors.New("environment name must be non-empty and exclude '=' and NUL")
		}
		if strings.IndexByte(entry.Value, 0) >= 0 {
			return fmt.Errorf("environment value for %q contains NUL", entry.Name)
		}
		if reservedOwnerEnvironment(entry.Name) {
			return fmt.Errorf("environment name %q is reserved for process-owner correlation or transport", entry.Name)
		}
		if index > 0 && compareEnvironmentNames(environment[index-1].Name, entry.Name) >= 0 {
			return errors.New("environment entries must be unique and sorted by ASCII-folded UTF-8 name")
		}
	}
	return nil
}

func IsReservedEnvironmentName(name string) bool {
	return reservedOwnerEnvironment(name)
}

func reservedOwnerEnvironment(name string) bool {
	for _, reserved := range [...]string{
		testtrace.EventFDEnvironment,
		testtrace.EventHandleEnvironment,
		testrun.RunIDEnvironment,
		testrun.OperationIDEnvironment,
		testrun.ScenarioEnvironment,
	} {
		if bytes.Equal(asciiFold(name), asciiFold(reserved)) {
			return true
		}
	}
	return false
}

func compareEnvironmentNames(left, right string) int {
	leftFolded := asciiFold(left)
	rightFolded := asciiFold(right)
	return bytes.Compare(leftFolded, rightFolded)
}

func asciiFold(value string) []byte {
	result := []byte(value)
	for index, character := range result {
		if character >= 'A' && character <= 'Z' {
			result[index] = character + ('a' - 'A')
		}
	}
	return result
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

func validateNFCText(label, value string, allowEmpty bool) error {
	if (!allowEmpty && value == "") || strings.IndexByte(value, 0) >= 0 ||
		!utf8.ValidString(value) || !norm.NFC.IsNormalString(value) {
		return fmt.Errorf("%s must be valid NFC text without NUL", label)
	}
	return nil
}

func validateDocumentSize(value any, label string) error {
	if _, err := EncodeCanonical(value); err != nil {
		return fmt.Errorf("%s exceeds its canonical document boundary: %w", label, err)
	}
	return nil
}

func EncodeCanonical(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := output.Bytes()
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		return nil, errors.New("JSON encoder omitted record terminator")
	}
	encoded = jsonStringifyCompatible(bytes.Clone(encoded[:len(encoded)-1]))
	if len(encoded) == 0 || len(encoded) > MaximumDocumentBytes {
		return nil, fmt.Errorf("canonical JSON length must be in [1, %d]", MaximumDocumentBytes)
	}
	return encoded, nil
}

func DecodeCanonical[T any](encoded []byte) (T, error) {
	var zero T
	if len(encoded) == 0 || len(encoded) > MaximumDocumentBytes {
		return zero, fmt.Errorf("canonical JSON length must be in [1, %d]", MaximumDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var decoded T
	if err := decoder.Decode(&decoded); err != nil {
		return zero, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return zero, errors.New("canonical JSON contains trailing data")
	}
	canonical, err := EncodeCanonical(decoded)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(encoded, canonical) {
		return zero, errors.New("JSON document is not canonical")
	}
	return decoded, nil
}

func ReadDocument[T any](reader io.Reader) (T, error) {
	var zero T
	encoded, err := io.ReadAll(io.LimitReader(reader, MaximumDocumentBytes+1))
	if err != nil {
		return zero, err
	}
	return DecodeCanonical[T](encoded)
}

func WriteDocument(writer io.Writer, value any) error {
	encoded, err := EncodeCanonical(value)
	if err != nil {
		return err
	}
	return writeAll(writer, encoded)
}

// ReadLineDocument reads the canonical JSONL form used by pipe transports.
// Requiring exactly one LF keeps the framing contract explicit instead of
// accidentally accepting the broader whitespace rules of encoding/json.
func ReadLineDocument[T any](reader io.Reader) (T, error) {
	var zero T
	encoded, err := io.ReadAll(io.LimitReader(reader, MaximumDocumentBytes+2))
	if err != nil {
		return zero, err
	}
	return DecodeLine[T](encoded)
}

func DecodeLine[T any](encoded []byte) (T, error) {
	var zero T
	if len(encoded) < 2 || len(encoded) > MaximumDocumentBytes+1 || encoded[len(encoded)-1] != '\n' {
		return zero, errors.New("canonical JSON line must contain one document followed by LF")
	}
	return DecodeCanonical[T](encoded[:len(encoded)-1])
}

// WriteLineDocument writes one canonical JSON document followed by one LF.
func WriteLineDocument(writer io.Writer, value any) error {
	encoded, err := EncodeCanonical(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeAll(writer, encoded)
}

func ReadFrame[T any](reader io.Reader) (T, error) {
	var zero T
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return zero, err
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || length > MaximumDocumentBytes {
		return zero, fmt.Errorf("frame length must be in [1, %d]", MaximumDocumentBytes)
	}
	encoded := make([]byte, int(length))
	if _, err := io.ReadFull(reader, encoded); err != nil {
		return zero, err
	}
	return DecodeCanonical[T](encoded)
}

func WriteFrame(writer io.Writer, value any) error {
	encoded, err := EncodeCanonical(value)
	if err != nil {
		return err
	}
	if len(encoded) == 0 || len(encoded) > MaximumDocumentBytes {
		return fmt.Errorf("frame length must be in [1, %d]", MaximumDocumentBytes)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(encoded)))
	if err := writeAll(writer, header); err != nil {
		return err
	}
	return writeAll(writer, encoded)
}

func jsonStringifyCompatible(encoded []byte) []byte {
	result := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); {
		if encoded[index] != '\\' {
			result = append(result, encoded[index])
			index++
			continue
		}
		runEnd := index
		for runEnd < len(encoded) && encoded[runEnd] == '\\' {
			runEnd++
		}
		runLength := runEnd - index
		if runLength%2 == 1 && runEnd+5 <= len(encoded) {
			escape := string(encoded[runEnd : runEnd+5])
			if escape == "u2028" || escape == "u2029" {
				result = append(result, encoded[index:runEnd-1]...)
				if escape == "u2028" {
					result = append(result, "\u2028"...)
				} else {
					result = append(result, "\u2029"...)
				}
				index = runEnd + 5
				continue
			}
		}
		result = append(result, encoded[index:runEnd]...)
		index = runEnd
	}
	return result
}

func writeAll(writer io.Writer, encoded []byte) error {
	for len(encoded) > 0 {
		written, err := writer.Write(encoded)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}
