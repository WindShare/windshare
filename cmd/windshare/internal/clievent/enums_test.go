package clievent

import "testing"

func TestClosedEnumRegistriesNameEveryDeclaredValue(t *testing.T) {
	tests := []struct {
		name  string
		first int
		last  int
		value func(int) (string, bool)
	}{
		{"command", int(CommandShare), int(CommandGet), func(value int) (string, bool) { return Command(value).Name() }},
		{"level", int(LevelDebug), int(LevelError), func(value int) (string, bool) { return Level(value).Name() }},
		{"transport", int(TransportRelay), int(TransportWebRTC), func(value int) (string, bool) { return Transport(value).Name() }},
		{"content path", int(ContentPathRelay), int(ContentPathDirectAndRelay), func(value int) (string, bool) { return ContentPath(value).Name() }},
		{"exit", int(ExitSuccess), int(ExitDrift), func(value int) (string, bool) { return ExitCode(value).Name() }},
		{"result", int(ResultSuccess), int(ResultFailed), func(value int) (string, bool) { return ResultStatus(value).Name() }},
		{"discovery", int(DiscoveryOpen), int(DiscoveryFailed), func(value int) (string, bool) { return DiscoveryStatus(value).Name() }},
		{"send disposition", int(SendAccepted), int(SendRetired), func(value int) (string, bool) { return SendDisposition(value).Name() }},
		{"channel state", int(ChannelConnecting), int(ChannelClosed), func(value int) (string, bool) { return ChannelState(value).Name() }},
		{"message key", int(MessageUnexpected), int(MessageOutputNeedsAttention), func(value int) (string, bool) { return SafeMessageKey(value).Name() }},
		{"fault domain", int(FaultSource), int(FaultCheckpoint), func(value int) (string, bool) { return FaultDomain(value).Name() }},
		{"fault scope", int(FaultFileLocal), int(FaultSessionTerminal), func(value int) (string, bool) { return FaultScope(value).Name() }},
		{"drift", int(DriftNone), int(DriftSource), func(value int) (string, bool) { return DriftReason(value).Name() }},
		{"relay scheme", int(RelayWS), int(RelayWSS), func(value int) (string, bool) { return RelayScheme(value).Name() }},
		{"sharing subject", int(SharingFile), int(SharingMultiple), func(value int) (string, bool) { return SharingSubjectKind(value).Name() }},
		{"relay recovery", int(RelayRecoveryStarted), int(RelayRecoveryFailed), func(value int) (string, bool) { return RelayRecoveryState(value).Name() }},
		{"trace incomplete", int(TraceIncompleteLifecycleDrop), int(TraceIncompleteSchemaLimit), func(value int) (string, bool) { return TraceIncompleteCause(value).Name() }},
		{"relay lifecycle stage", int(RelayTerminalReserved), int(RelayTraceDropped), func(value int) (string, bool) { return RelayLifecycleStage(value).Name() }},
		{"relay retirement", int(RelayRetirementNone), int(RelayRetirementIngressFailure), func(value int) (string, bool) { return RelayRetirementSource(value).Name() }},
		{"relay cause", int(RelayCauseNone), int(RelayCauseTransport), func(value int) (string, bool) { return RelayLifecycleCause(value).Name() }},
		{"webrtc operation", int(WebRTCChannel), int(WebRTCSendTerminal), func(value int) (string, bool) { return WebRTCOperation(value).Name() }},
		{"webrtc transition", int(WebRTCSendAccepted), int(WebRTCTraceDropped), func(value int) (string, bool) { return WebRTCTransition(value).Name() }},
		{"webrtc terminal", int(WebRTCTerminalNone), int(WebRTCTerminalRemotePending), func(value int) (string, bool) { return WebRTCTerminalState(value).Name() }},
		{"webrtc cause", int(WebRTCCauseNone), int(WebRTCCauseOther), func(value int) (string, bool) { return WebRTCLifecycleCause(value).Name() }},
		{"peer attempt", int(PeerAttemptStarted), int(PeerAttemptFailed), func(value int) (string, bool) { return PeerAttemptStage(value).Name() }},
		{"peer scope", int(PeerFailureAttempt), int(PeerFailureSession), func(value int) (string, bool) { return PeerFailureScope(value).Name() }},
		{"transfer stage", int(TransferDiscoveryStarted), int(TransferJobSettled), func(value int) (string, bool) { return TransferLifecycleStage(value).Name() }},
		{"file selection", int(FileSelectionNone), int(FileSelectionCatalogPathTarget), func(value int) (string, bool) { return FileSelectionDecision(value).Name() }},
		{"file settlement", int(FileSettlementNone), int(FileFailed), func(value int) (string, bool) { return FileSettlement(value).Name() }},
		{"tree settlement", int(TreeSettlementNone), int(TreeSettlementFailed), func(value int) (string, bool) { return TreeSettlement(value).Name() }},
		{"filesystem operation", int(FilesystemCertified), int(FilesystemRuntimeDecision), func(value int) (string, bool) { return FilesystemOutputOperation(value).Name() }},
		{"sender terminal transport", int(SenderTerminalAccepted), int(SenderTerminalRetired), func(value int) (string, bool) { return SenderTerminalTransport(value).Name() }},
		{"sender terminal outcome", int(SenderTerminalDelivered), int(SenderTerminalUnknown), func(value int) (string, bool) { return SenderTerminalOutcome(value).Name() }},
		{"sender terminal decision", int(SenderTerminalDecisionDelivered), int(SenderTerminalFailed), func(value int) (string, bool) { return SenderTerminalDecision(value).Name() }},
		{"catalog operation", int(CatalogStorageCreating), int(CatalogStorageCleaned), func(value int) (string, bool) { return CatalogStorageOperation(value).Name() }},
		{"catalog cause", int(CatalogStorageCauseNone), int(CatalogStorageCauseUnexpected), func(value int) (string, bool) { return CatalogStorageCause(value).Name() }},
		{"root prefetch", int(RootPrefetchAttemptStarted), int(RootPrefetchStopped), func(value int) (string, bool) { return RootPrefetchDecision(value).Name() }},
		{"protocol role", int(ProtocolRoleReceiver), int(ProtocolRoleSender), func(value int) (string, bool) { return ProtocolRole(value).Name() }},
		{"protocol operation stage", int(ProtocolOperationReceiverCompleted), int(ProtocolOperationSenderResponseSettled), func(value int) (string, bool) { return ProtocolOperationStage(value).Name() }},
		{"protocol message", int(ProtocolMessageListChildren), int(ProtocolMessagePeerCandidate), func(value int) (string, bool) { return ProtocolMessageKind(value).Name() }},
		{"protocol send outcome", int(ProtocolSendUnknown), int(ProtocolSendDropped), func(value int) (string, bool) { return ProtocolSendOutcome(value).Name() }},
		{"protocol operation cause", int(ProtocolOperationCauseNone), int(ProtocolOperationCauseProtocolFailure), func(value int) (string, bool) { return ProtocolOperationCause(value).Name() }},
		{"lane route", int(LaneRouteRelay), int(LaneRouteDirect), func(value int) (string, bool) { return LaneRoute(value).Name() }},
		{"observer loss category", int(ObserverLossRelayLifecycle), int(ObserverLossCommandAdapter), func(value int) (string, bool) { return ObserverLossCategory(value).Name() }},
		{"observer loss reason", int(ObserverLossUnknownEnum), int(ObserverLossCleanupResidue), func(value int) (string, bool) { return ObserverLossReason(value).Name() }},
		{"receiver terminal owner", int(ReceiverTerminalUnbound), int(ReceiverTerminalRuntime), func(value int) (string, bool) { return ReceiverTerminalOwner(value).Name() }},
		{"receiver disposition", int(ReceiverFallbackAllowed), int(ReceiverSessionUnsafe), func(value int) (string, bool) { return ReceiverDisposition(value).Name() }},
		{"receiver provenance", int(ReceiverProvenanceUnbound), int(ReceiverProvenanceAuthenticatedContinuationAuthorityViolation), func(value int) (string, bool) { return ReceiverProvenance(value).Name() }},
		{"receiver local stop", int(ReceiverLocalStopNone), int(ReceiverLocalStopNormalCompletion), func(value int) (string, bool) { return ReceiverLocalStopReason(value).Name() }},
		{"receiver benign", int(ReceiverBenignContextCanceled), int(ReceiverBenignRemoteFinalOperationMissing), func(value int) (string, bool) { return ReceiverBenignComponent(value).Name() }},
		{"receiver cause class", int(ReceiverCauseRuntimeClosed), int(ReceiverCauseUnknown), func(value int) (string, bool) { return ReceiverCauseClass(value).Name() }},
		{"peer teardown", int(PeerTeardownShutdownInitiated), int(PeerTeardownChannelDrainJoined), func(value int) (string, bool) { return PeerTeardownTransition(value).Name() }},
		{"filesystem failure stage", int(FilesystemFailureDestinationBinding), int(FilesystemFailureAuthorityClose), func(value int) (string, bool) { return FilesystemFailureStage(value).Name() }},
		{"filesystem reconciliation", int(FilesystemReconciliationCandidateObservation), int(FilesystemReconciliationRecordPromotion), func(value int) (string, bool) { return FilesystemReconciliationStep(value).Name() }},
		{"filesystem native error", int(FilesystemNativeErrorAccessDenied), int(FilesystemNativeErrorUnknown), func(value int) (string, bool) { return FilesystemNativeErrorClass(value).Name() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for value := test.first; value <= test.last; value++ {
				name, ok := test.value(value)
				if !ok || name == "" {
					t.Fatalf("enum value %d has no stable name", value)
				}
			}
			for _, value := range []int{test.first - 1, test.last + 1} {
				if name, ok := test.value(value); ok || name != "" {
					t.Fatalf("unknown enum value %d mapped to %q", value, name)
				}
			}
		})
	}
	for code := ExitSuccess; code <= ExitDrift; code++ {
		if process, ok := code.ProcessCode(); !ok || process != int(code) || !code.Valid() {
			t.Fatalf("exit code %d process projection = %d,%t", code, process, ok)
		}
	}
	if _, ok := ExitCode(255).ProcessCode(); ok {
		t.Fatal("unknown exit code has a process code")
	}
}

func TestSharingSubjectsExposeOnlyShapeAppropriateDisplayFacts(t *testing.T) {
	file, err := NewFileSubject(NewDisplayName("report.bin"), 42)
	if err != nil || !file.Valid() || file.Kind() != SharingFile ||
		file.Name().Text() != "report.bin" || file.FileBytes() != 42 || file.SelectedItems() != 1 {
		t.Fatalf("file subject = %+v err=%v", file, err)
	}
	directory, err := NewDirectorySubject(NewDisplayName("photos"))
	if err != nil || !directory.Valid() || directory.Kind() != SharingDirectory || directory.FileBytes() != 0 {
		t.Fatalf("directory subject = %+v err=%v", directory, err)
	}
	multiple, err := NewMultipleSubject(3)
	if err != nil || !multiple.Valid() || multiple.Kind() != SharingMultiple ||
		!multiple.Name().Empty() || multiple.SelectedItems() != 3 {
		t.Fatalf("multiple subject = %+v err=%v", multiple, err)
	}
	if _, err := NewFileSubject(DisplayName{}, 1); err == nil {
		t.Fatal("accepted unnamed file subject")
	}
	if _, err := NewDirectorySubject(DisplayName{}); err == nil {
		t.Fatal("accepted unnamed directory subject")
	}
	if _, err := NewMultipleSubject(1); err == nil {
		t.Fatal("accepted single-item multiple subject")
	}
}
