package sessionobs

import (
	"testing"
)

func TestSenderTerminalPairsAreClosed(t *testing.T) {
	pairs := []struct {
		trigger    SenderSessionTerminalTrigger
		provenance SenderSessionTerminalProvenance
	}{
		{SenderSessionTerminalTriggerGracefulStop, SenderSessionTerminalProvenanceNormalStop},
		{SenderSessionTerminalTriggerForcedClose, SenderSessionTerminalProvenanceCallerStop},
		{SenderSessionTerminalTriggerPeerTerminal, SenderSessionTerminalProvenanceRemoteClose},
		{SenderSessionTerminalTriggerPathsExhausted, SenderSessionTerminalProvenanceLaneRetirement},
		{SenderSessionTerminalTriggerRuntimeFailed, SenderSessionTerminalProvenanceLocalFault},
	}
	for triggerIndex, trigger := range pairs {
		for provenanceIndex, provenance := range pairs {
			valid := validSenderSessionTerminalPair(trigger.trigger, provenance.provenance)
			if valid != (triggerIndex == provenanceIndex) {
				t.Fatalf(
					"pair %q/%q validity=%t, want %t",
					trigger.trigger,
					provenance.provenance,
					valid,
					triggerIndex == provenanceIndex,
				)
			}
		}
	}
	if validSenderSessionTerminalPair("unknown", SenderSessionTerminalProvenanceNormalStop) {
		t.Fatal("unknown trigger was accepted")
	}
	if validSenderSessionTerminalPair(SenderSessionTerminalTriggerGracefulStop, "unknown") {
		t.Fatal("unknown provenance was accepted")
	}
	if (SenderSessionTerminated{
		Trigger:    SenderSessionTerminalTriggerGracefulStop,
		Provenance: SenderSessionTerminalProvenanceNormalStop,
	}).Valid() {
		t.Fatal("zero protocol session identity was accepted")
	}
}
