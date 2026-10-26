package sessioncore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestResponseWithoutLaneAndMalformedFailureReleaseRoute(t *testing.T) {
	sender, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	identity := sender.initial
	outbound := NewResponses(sender, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)), nil)
	fragments, err := contentflow.FragmentRecord(id16[protocolsession.OperationID](77), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	sender.lanes.detach(identity)
	if err := outbound.SendFragment(context.Background(), fragments[0]); !errors.Is(err, sessioncontract.ErrOperationMissing) {
		t.Fatalf("fragment without lane error = %v", err)
	}
	invalidFailureID := id16[protocolsession.OperationID](76)
	invalidFailureRoute := sender.routes.reserve(invalidFailureID, identity)
	if invalidFailureRoute == nil {
		t.Fatal("failed to reserve invalid failure route")
	}
	invalidFailureContext := bindOutboundRoute(context.Background(), invalidFailureID, invalidFailureRoute)
	if err := outbound.SendOperationError(invalidFailureContext, invalidFailureID, contentflow.OperationFailure{}); err == nil {
		t.Fatal("invalid operation failure was sent")
	}
	sender.routes.mu.Lock()
	_, retainedInvalidFailure := sender.routes.routes[invalidFailureID]
	sender.routes.mu.Unlock()
	if retainedInvalidFailure {
		t.Fatal("invalid operation failure retained its lane route")
	}
}
