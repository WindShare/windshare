package sessioncore

import (
	"fmt"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestTerminationClaimsAreRoleNeutralAndPublishOnceAcrossCopies(t *testing.T) {
	for _, role := range []protocolsession.Role{protocolsession.RoleSender, protocolsession.RoleReceiver} {
		t.Run(fmt.Sprint(role), func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, role)
			observed := make(chan Termination, 8)
			var claim TerminationClaim
			runtime.terminationObserver = TerminationObserverFunc(func(event Termination) {
				observed <- event
				// A callback may reenter publication without deadlocking or emitting
				// another root, including when callers retained copies of the claim.
				claim.Publish()
			})
			claim.Publish()
			runtime.ClaimTermination(0).Publish()
			runtime.ClaimTermination(TerminationCause(255)).Publish()
			claim = runtime.ClaimTermination(TerminationForcedClose)
			const publishers = 4
			var workers sync.WaitGroup
			for range publishers {
				copied := claim
				workers.Go(copied.Publish)
			}
			workers.Wait()
			event := awaitTermination(t, observed)
			assertTermination(t, event, runtime.sessionID, TerminationForcedClose)
			assertNoTermination(t, observed)
			if runtime.ctx.Err() != nil {
				t.Fatal("publishing a diagnostic claim canceled the runtime")
			}
			runtime.BeginClose()
			if runtime.ctx.Err() == nil {
				t.Fatal("losing close request failed to cancel lifetime")
			}
			assertNoTermination(t, observed)
		})
	}
}
