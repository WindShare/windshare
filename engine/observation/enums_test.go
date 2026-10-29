package observation

import (
	"testing"
)

func TestClosedEnumRegistriesNameEveryDeclaredValue(t *testing.T) {
	tests := []struct {
		name  string
		first int
		last  int
		value func(int) (string, bool)
	}{
		{"level", int(LevelDebug), int(LevelError), func(value int) (string, bool) { return Level(value).Name() }},
		{"transport", int(TransportRelay), int(TransportWebRTC), func(value int) (string, bool) { return Transport(value).Name() }},
		{"content path", int(ContentPathRelay), int(ContentPathDirectAndRelay), func(value int) (string, bool) { return ContentPath(value).Name() }},
		{"discovery", int(DiscoveryOpen), int(DiscoveryFailed), func(value int) (string, bool) { return DiscoveryStatus(value).Name() }},
		{"message key", int(MessageUnexpected), int(MessageRetainedOutputMissing), func(value int) (string, bool) { return SafeMessageKey(value).Name() }},
		{"fault domain", int(FaultSource), int(FaultCheckpoint), func(value int) (string, bool) { return FaultDomain(value).Name() }},
		{"fault scope", int(FaultFileLocal), int(FaultSessionTerminal), func(value int) (string, bool) { return FaultScope(value).Name() }},
		{"relay scheme", int(RelayWS), int(RelayWSS), func(value int) (string, bool) { return RelayScheme(value).Name() }},
		{"sharing subject", int(SharingFile), int(SharingMultiple), func(value int) (string, bool) { return SharingSubjectKind(value).Name() }},
		{"relay recovery", int(RelayRecoveryStarted), int(RelayRecoveryWaiting), func(value int) (string, bool) { return RelayRecoveryState(value).Name() }},
		{"observer loss category", int(ObserverLossRelayLifecycle), int(ObserverLossCategoryLimit - 1), func(value int) (string, bool) { return ObserverLossCategory(value).Name() }},
		{"observer loss reason", int(ObserverLossUnknownEnum), int(ObserverLossCleanupResidue), func(value int) (string, bool) { return ObserverLossReason(value).Name() }},
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
