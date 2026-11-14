package commandprojection

import (
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestStartupFailuresHaveCommandIndependentStatus(t *testing.T) {
	for _, command := range []clievent.Command{clievent.CommandShare, clievent.CommandGet} {
		for _, test := range []struct {
			cause error
			exit  clievent.ExitCode
			code  appobs.FailureCode
		}{
			{engine.ErrNetworkUnavailable, clievent.ExitNetwork, appobs.FailureNetworkUnavailable},
			{fmt.Errorf("startup: %w", errors.Join(engine.ErrNetworkUnavailable, errors.New("directory trust unavailable"))),
				clievent.ExitNetwork, appobs.FailureNetworkUnavailable},
			{relayprofile.ErrCandidatesRequired, clievent.ExitUsage, appobs.FailureInvalidInput},
			{engine.ErrInvalidShareChunkSize, clievent.ExitUsage, appobs.FailureInvalidInput},
			{errors.New("unknown startup failure"), clievent.ExitFailure, appobs.FailureUnexpected},
			{nil, clievent.ExitFailure, appobs.FailureUnexpected},
		} {
			event, err := ProjectStartFailure(command, test.cause)
			if err != nil || clievent.CommandOf(event) != command || event.ExitCode() != test.exit || event.Failure().Code() != test.code {
				t.Fatalf("command=%v cause=%v event=%+v err=%v", command, test.cause, event, err)
			}
		}
	}
	if _, err := ProjectStartFailure(0, engine.ErrNetworkUnavailable); !errors.Is(err, ErrInvalidProjection) {
		t.Fatalf("invalid command accepted: %v", err)
	}
}

func TestSettledFailureStatusRemainsAuthoritative(t *testing.T) {
	event, err := ProjectCommandFailure(clievent.CommandShare, clievent.ExitFailure, engine.ErrNetworkUnavailable)
	if err != nil || event.ExitCode() != clievent.ExitFailure || event.Failure().Code() != appobs.FailureNetworkUnavailable {
		t.Fatalf("startup policy replaced the settled outcome: %+v %v", event, err)
	}
}
