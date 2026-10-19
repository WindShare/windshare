package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/internal/testoutputroot"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	v2ExplicitStopScenario           = "v2-explicit-stop-tombstone"
	v2SenderSessionRetiredMilestone  = "sender_session_retired"
	v2ReceiverJoinStoppedMilestone   = "receiver_join_stopped"
	v2StoppedCandidateCheckMilestone = "stopped_candidate_check"
)

func TestLongV2ProcessExplicitStopPublishesDurableRelayTombstone(t *testing.T) {
	requireV2ProcessScenario(t)
	scenario := startV2Scenario(t, v2ExplicitStopScenario)
	binaries := loadE2EBinaries(t)
	fixtures := []*v2RelayFixture{
		newV2RelayFixture(t, scenario, ""),
		newV2RelayFixture(t, scenario, ""),
	}
	relays := []*v2RelayProcess{fixtures[0].start(t), fixtures[1].start(t)}
	source := filepath.Join(t.TempDir(), "stopped-share.txt")
	payload := []byte("must never be available after STOP")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	share := startTracedV2Process(
		t, scenario, v2ShareCommandComponent, binaries.wind,
		"share", source, "--front-url", v2FrontendBaseURL, "--relay", relays[0].baseURL, "--relay", relays[1].baseURL,
	)
	shareLink := waitV2Match(t, share, regexp.MustCompile(`(?m)^Link: (\S+)$`), share.stdout)
	capability, err := link.Parse(shareLink)
	if err != nil {
		t.Fatal(err)
	}
	if len(capability.Relays) != len(relays) {
		t.Fatalf("advertised relays=%v", capability.Relays)
	}
	capabilitySecrets := v2CapabilityForbiddenValues(shareLink)
	share.forbidStderr(capabilitySecrets...)
	share.forbidUserTrace(append(capabilitySecrets, source, filepath.Base(source))...)
	candidateLinks := make([]string, len(relays))
	for index, relay := range relays {
		candidate := capability
		candidate.Relays = []string{relay.baseURL}
		candidateLinks[index], err = candidate.URL("http://" + relay.address)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(candidateLinks[index], "?") {
			t.Fatal("same-origin receiver scenario did not produce a compact link")
		}
		output := testoutputroot.New(t).RootPath
		arguments := []string{"get", candidateLinks[index], "-o", output, "--connectivity", "relay-only"}
		if index > 0 {
			arguments = append(arguments, "--only", filepath.Base(source))
		}
		receiver := startTracedV2Process(t, scenario, v2GetCommandComponent, binaries.wind, arguments...)
		receiver.forbidStderr(capabilitySecrets...)
		receiver.forbidUserTrace(append(capabilitySecrets, source, filepath.Base(source), output)...)
		if err := receiver.wait(t); err != nil {
			t.Fatalf("relay %s was not serving before STOP: %v; stderr=%q", relay.baseURL, err, receiver.stderr.String())
		}
		assertV2File(t, filepath.Join(output, filepath.Base(source)), payload)
		events := drainV2ProcessTraces(t, receiver)
		requireV2EventCount(t, events, v2ReceiverRelayContentMilestone, testrun.OutcomeSucceeded, 1)
		waitV2ProcessTrace(t, share, v2SenderSessionRetiredMilestone, testrun.OutcomeSucceeded)
	}
	share.interrupt(t)
	waitV2ProcessTrace(t, share, v2SenderStopMilestone, testrun.OutcomeSucceeded)
	if err := share.wait(t); err != nil {
		t.Fatalf("sender failed while committing STOP: %v; stderr=%q", err, share.stderr.String())
	}

	for _, restarted := range []bool{false, true} {
		if restarted {
			for _, relay := range relays {
				relay.stop(t)
			}
			for index, fixture := range fixtures {
				restartedRelay := fixture.start(t)
				if restartedRelay.baseURL != relays[index].baseURL {
					t.Fatalf("relay identity changed across restart: %s -> %s", relays[index].baseURL, restartedRelay.baseURL)
				}
				relays[index] = restartedRelay
			}
		}
		for index, candidateLink := range candidateLinks {
			phaseContext := struct {
				Endpoint  string `json:"endpoint"`
				Restarted bool   `json:"restarted"`
			}{relays[index].baseURL, restarted}
			phase := scenario.startPhase(t, v2StoppedCandidateCheckMilestone, phaseContext)
			output := testoutputroot.New(t).RootPath
			receiver := startTracedV2Process(
				t, scenario, v2GetCommandComponent, binaries.wind,
				"get", candidateLink, "-o", output, "--connectivity", "relay-only",
			)
			receiver.forbidStderr(capabilitySecrets...)
			receiver.forbidUserTrace(append(capabilitySecrets, source, filepath.Base(source), output)...)
			if err := receiver.wait(t); err == nil {
				t.Fatalf("relay %s accepted JOIN after STOP (restarted=%v)", relays[index].baseURL, restarted)
			}
			events := drainV2ProcessTraces(t, receiver)
			requireV2EventCount(t, events, v2ReceiverJoinStoppedMilestone, testrun.OutcomeFailed, 1)
			scenario.succeedPhase(t, phase, phaseContext)
		}
	}
	scenario.requireSuccess(t)
}
