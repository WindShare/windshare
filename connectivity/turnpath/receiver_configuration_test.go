package turnpath

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/session/turncontrol"
)

func TestReceiverInventoryRevisionReplacesServicesWithoutResettingDemand(t *testing.T) {
	r := NewReceiver(ReceiverConfig{SessionID: [16]byte{1},
		Send: func(_ context.Context, _ []byte) error { return nil },
		Open: func(_ context.Context, _ turncontrol.Grant, _ iceconfig.Connection) (ReceiverPath, error) {
			return &receiverFixturePath{ready: make(chan struct{}), done: make(chan struct{})}, nil
		},
	})
	ctx := t.Context()
	owner := turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 2}
	r.apply(ctx, turncontrol.Available{Authority: owner, Profiles: []string{"primary"}})
	r.SetDemand(true)
	r.maintain(ctx)
	grant := turncontrol.Grant{Authority: owner, PathID: [16]byte{3}, Provider: "test", Account: "owner", Profile: "primary",
		CredentialKind: turncontrol.Expiring, Username: "user", Password: "password", ValidFor: time.Minute, URLs: []string{"turn:localhost"}}
	grant.Authority.Sequence = r.sequence
	r.apply(ctx, grant)
	old := r.paths[grant.PathID]
	if old == nil {
		t.Fatal("initial grant missing")
	}
	owner.Sequence++
	r.apply(ctx, turncontrol.Available{Authority: owner, Profiles: []string{"secondary"}})
	if len(r.paths) != 0 || r.sequence != grant.Authority.Sequence {
		t.Fatal("inventory update retained removed path or reset demand")
	}
	select {
	case <-old.path.Done():
	default:
		t.Fatal("removed service not closed")
	}
	stale := owner
	stale.Sequence--
	r.apply(ctx, turncontrol.Available{Authority: stale, Profiles: []string{"primary"}})
	foreign := owner
	foreign.OwnerPathID[0]++
	foreign.Sequence++
	r.apply(ctx, turncontrol.Available{Authority: foreign, Profiles: []string{"primary"}})
	if r.profiles["primary"] || !r.profiles["secondary"] {
		t.Fatal("stale or foreign inventory replaced current configuration")
	}
	grant.PathID[0]++
	grant.Profile = "secondary"
	r.apply(ctx, grant)
	if len(r.paths) != 1 {
		t.Fatal("new inventory not usable by existing demand")
	}
	owner.Sequence++
	r.apply(ctx, turncontrol.Available{Authority: owner})
	if len(r.paths) != 0 || len(r.profiles) != 0 {
		t.Fatal("empty inventory failed to withdraw authority")
	}
}
