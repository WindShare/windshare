package diagnostic

import (
	"strconv"

	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func (event FilesystemOutputObserved) EncodeObservation() (wire.Encoded, error) {
	if err := event.Validate(); err != nil {
		return wire.Encoded{}, err
	}
	payload, err := projectFilesystemOutput(event)
	if err != nil {
		return wire.Encoded{}, err
	}
	return wire.Encoded{Event: "filesystem_output", Payload: payload}, nil
}

type namedValue interface{ Name() (string, bool) }

func nameOf(value namedValue) (string, error) {
	name, ok := value.Name()
	if !ok {
		return "", appobs.ErrInvalidEvent
	}
	return name, nil
}
func namedPointer(value namedValue) (*string, error) {
	name, err := nameOf(value)
	if err != nil {
		return nil, err
	}
	return &name, nil
}
func decimal(value uint64) string         { return strconv.FormatUint(value, 10) }
func decimalPointer(value uint64) *string { encoded := decimal(value); return &encoded }

type FilesystemNativeLockPayload struct {
	Scope     string `json:"scope"`
	Milestone string `json:"milestone"`
}

type FilesystemRuntimeDecisionPayload struct {
	Component string `json:"component"`
	Operation string `json:"operation"`
	Decision  string `json:"decision"`
}

type FilesystemCorrelation struct {
	OperationID *string `json:"operation_id,omitempty"`
	ClaimID     *string `json:"claim_id,omitempty"`
}

type FilesystemCounters struct {
	NodeClaims             string `json:"node_claims"`
	DirectoryClaims        string `json:"directory_claims"`
	FileClaims             string `json:"file_claims"`
	ActiveFileClaims       string `json:"active_file_claims"`
	ReservedFileSlots      string `json:"reserved_file_slots"`
	DirectoryMetadataBytes string `json:"directory_metadata_bytes"`
	CheckpointRecords      string `json:"checkpoint_records"`
}

type FilesystemFailure struct {
	Stage              string       `json:"stage,omitempty"`
	ReconciliationStep *string      `json:"reconciliation_step,omitempty"`
	NativeErrorClass   *string      `json:"native_error_class,omitempty"`
	Failure            wire.Failure `json:"failure"`
}

type FilesystemCapabilityPayload struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

type FilesystemCapabilities struct {
	Mode              string                      `json:"mode"`
	SafePublish       FilesystemCapabilityPayload `json:"safe_publish"`
	OperationRecovery FilesystemCapabilityPayload `json:"operation_recovery"`
	RangeRecovery     FilesystemCapabilityPayload `json:"range_recovery"`
	CrashCleanup      FilesystemCapabilityPayload `json:"crash_cleanup"`
}

type FilesystemOutputPayload struct {
	Capabilities        *FilesystemCapabilities           `json:"capabilities,omitempty"`
	Operation           string                            `json:"operation"`
	ReceiveOperationID  *string                           `json:"receive_operation_id,omitempty"`
	ReceiveIntentDigest *string                           `json:"receive_intent_digest,omitempty"`
	OutputSessionID     *string                           `json:"output_session_id,omitempty"`
	Certification       *string                           `json:"certification,omitempty"`
	NativeLock          *FilesystemNativeLockPayload      `json:"native_lock,omitempty"`
	RootDisposition     *string                           `json:"root_disposition,omitempty"`
	RuntimeDecision     *FilesystemRuntimeDecisionPayload `json:"runtime_decision,omitempty"`
	CheckpointDecision  *string                           `json:"checkpoint_decision,omitempty"`
	Correlation         *FilesystemCorrelation            `json:"output_correlation,omitempty"`
	Counters            FilesystemCounters                `json:"counters"`
	Failure             *FilesystemFailure                `json:"failure,omitempty"`
}

func (FilesystemOutputPayload) ObservationPayload() {}

type FilesystemOutputIdentities struct {
	receiveOperationID  *string
	receiveIntentDigest *string
	outputSessionID     *string
}

type FilesystemOutputAuthority struct {
	certification   *string
	nativeLock      *FilesystemNativeLockPayload
	rootDisposition *string
}

func projectFilesystemOutput(
	event FilesystemOutputObserved,
) (FilesystemOutputPayload, error) {
	operation, err := nameOf(event.Operation())
	if err != nil {
		return FilesystemOutputPayload{}, err
	}
	authority, err := projectFilesystemOutputAuthority(event)
	if err != nil {
		return FilesystemOutputPayload{}, err
	}
	runtimeDecision, err := projectFilesystemRuntimeDecisionPayload(event)
	if err != nil {
		return FilesystemOutputPayload{}, err
	}
	checkpointDecision, hasCheckpointDecision := event.CheckpointDecision()
	projectedCheckpointDecision, err := projectOptionalNamedValue(
		checkpointDecision,
		hasCheckpointDecision,
	)
	if err != nil {
		return FilesystemOutputPayload{}, err
	}
	failure, err := projectFilesystemOutputFailure(event)
	if err != nil {
		return FilesystemOutputPayload{}, err
	}
	identities := projectFilesystemOutputIdentities(event)
	return FilesystemOutputPayload{
		Capabilities:        projectFilesystemCapabilities(event),
		Operation:           operation,
		ReceiveOperationID:  identities.receiveOperationID,
		ReceiveIntentDigest: identities.receiveIntentDigest,
		OutputSessionID:     identities.outputSessionID,
		Certification:       authority.certification,
		NativeLock:          authority.nativeLock,
		RootDisposition:     authority.rootDisposition,
		RuntimeDecision:     runtimeDecision,
		CheckpointDecision:  projectedCheckpointDecision,
		Correlation:         projectFilesystemOutputCorrelation(event),
		Counters:            projectFilesystemCounters(event.Counters()),
		Failure:             failure,
	}, nil
}

func projectFilesystemCapabilities(event FilesystemOutputObserved) *FilesystemCapabilities {
	capabilities, present := event.DestinationCapabilities()
	if !present {
		return nil
	}
	project := func(value FilesystemCapability) FilesystemCapabilityPayload {
		reason, _ := value.Reason.Name()
		return FilesystemCapabilityPayload{Supported: value.Supported, Reason: reason}
	}
	mode, _ := capabilities.Mode.Name()
	return &FilesystemCapabilities{
		Mode: mode, SafePublish: project(capabilities.SafePublish),
		OperationRecovery: project(capabilities.OperationRecovery),
		RangeRecovery:     project(capabilities.RangeRecovery), CrashCleanup: project(capabilities.CrashCleanup),
	}
}

func projectFilesystemOutputIdentities(
	event FilesystemOutputObserved,
) FilesystemOutputIdentities {
	identities := FilesystemOutputIdentities{}
	if receiveOperation, ok := event.ReceiveOperationID(); ok {
		encoded := wire.EncodeIdentity(receiveOperation.Bytes())
		identities.receiveOperationID = &encoded
	}
	if receiveIntent, ok := event.ReceiveIntentDigest(); ok {
		encoded := receiveIntent.Hex()
		identities.receiveIntentDigest = &encoded
	}
	if outputSession, ok := event.OutputSessionID(); ok {
		encoded := wire.EncodeIdentity(outputSession.Bytes())
		identities.outputSessionID = &encoded
	}
	return identities
}

func projectFilesystemOutputAuthority(
	event FilesystemOutputObserved,
) (FilesystemOutputAuthority, error) {
	certification, hasCertification := event.Certification()
	projectedCertification, err := projectOptionalNamedValue(certification, hasCertification)
	if err != nil {
		return FilesystemOutputAuthority{}, err
	}
	nativeLock, err := projectFilesystemNativeLockPayload(event)
	if err != nil {
		return FilesystemOutputAuthority{}, err
	}
	rootDisposition, hasRootDisposition := event.RootDisposition()
	projectedRootDisposition, err := projectOptionalNamedValue(rootDisposition, hasRootDisposition)
	if err != nil {
		return FilesystemOutputAuthority{}, err
	}
	return FilesystemOutputAuthority{
		certification:   projectedCertification,
		nativeLock:      nativeLock,
		rootDisposition: projectedRootDisposition,
	}, nil
}

func projectOptionalNamedValue(value namedValue, present bool) (*string, error) {
	if !present {
		return nil, nil
	}
	return namedPointer(value)
}

func projectFilesystemNativeLockPayload(
	event FilesystemOutputObserved,
) (*FilesystemNativeLockPayload, error) {
	scope, milestone, present := event.NativeLock()
	if !present {
		return nil, nil
	}
	scopeName, err := nameOf(scope)
	if err != nil {
		return nil, err
	}
	milestoneName, err := nameOf(milestone)
	if err != nil {
		return nil, err
	}
	return &FilesystemNativeLockPayload{
		Scope: scopeName, Milestone: milestoneName,
	}, nil
}

func projectFilesystemRuntimeDecisionPayload(
	event FilesystemOutputObserved,
) (*FilesystemRuntimeDecisionPayload, error) {
	component, operation, decision, present := event.RuntimeDecision()
	if !present {
		return nil, nil
	}
	componentName, err := nameOf(component)
	if err != nil {
		return nil, err
	}
	operationName, err := nameOf(operation)
	if err != nil {
		return nil, err
	}
	decisionName, err := nameOf(decision)
	if err != nil {
		return nil, err
	}
	return &FilesystemRuntimeDecisionPayload{
		Component: componentName, Operation: operationName, Decision: decisionName,
	}, nil
}

func projectFilesystemOutputCorrelation(
	event FilesystemOutputObserved,
) *FilesystemCorrelation {
	operationID, claimID := event.Correlation()
	if operationID == 0 && claimID == 0 {
		return nil
	}
	return &FilesystemCorrelation{
		OperationID: optionalDecimalPointer(operationID),
		ClaimID:     optionalDecimalPointer(claimID),
	}
}

func optionalDecimalPointer(value uint64) *string {
	if value == 0 {
		return nil
	}
	return decimalPointer(value)
}

func projectFilesystemOutputFailure(
	event FilesystemOutputObserved,
) (*FilesystemFailure, error) {
	failure, present := event.Failure()
	if !present {
		return nil, nil
	}
	stage, reconciliation, nativeClass, classified := event.FailureClassification()
	var stageName string
	if classified {
		var err error
		stageName, err = nameOf(stage)
		if err != nil {
			return nil, err
		}
	}
	projectedFailure, err := wire.EncodeFailure(failure)
	if err != nil {
		return nil, err
	}
	return &FilesystemFailure{
		Stage:              stageName,
		ReconciliationStep: optionalClosedName(reconciliation),
		NativeErrorClass:   optionalClosedName(nativeClass),
		Failure:            projectedFailure,
	}, nil
}

func optionalClosedName(value namedValue) *string {
	name, present := value.Name()
	if !present {
		return nil
	}
	return &name
}

func projectFilesystemCounters(counters FilesystemOutputCounters) FilesystemCounters {
	return FilesystemCounters{
		NodeClaims:             decimal(counters.NodeClaims),
		DirectoryClaims:        decimal(counters.DirectoryClaims),
		FileClaims:             decimal(counters.FileClaims),
		ActiveFileClaims:       decimal(counters.ActiveFileClaims),
		ReservedFileSlots:      decimal(counters.ReservedFileSlots),
		DirectoryMetadataBytes: decimal(counters.DirectoryMetadataBytes),
		CheckpointRecords:      decimal(counters.CheckpointRecords),
	}
}
