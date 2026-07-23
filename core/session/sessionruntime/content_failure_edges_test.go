package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

type invalidRevisionRecordOpener struct{}

func (invalidRevisionRecordOpener) OpenRevision(
	catalog.FileID,
	uint32,
	[]byte,
) (content.FileRevisionDescriptor, error) {
	return content.FileRevisionDescriptor{}, nil
}

func (invalidRevisionRecordOpener) OpenBlock(
	content.FileRevisionDescriptor,
	uint64,
	[]byte,
) (records.BlockRecord, error) {
	return records.BlockRecord{}, errors.New("block opening is outside this test")
}

func TestOpenRevisionCompensatesLocallyMalformedDescriptorIdentity(t *testing.T) {
	fixture := newVerticalFixture(t)
	released := make(chan content.LeaseID, 1)
	fixture.contentStore.released = released
	receiverConfig := fixture.receiverConfig
	receiverConfig.RecordOpener = invalidRevisionRecordOpener{}
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	if _, err := receiver.OpenRevision(context.Background(), fixture.fileID); !errors.Is(err, transfer.ErrRevisionIdentity) {
		t.Fatalf("malformed local descriptor error=%v", err)
	}
	select {
	case leaseID := <-released:
		if leaseID != fixture.contentStore.lease.ID() {
			t.Fatalf("compensated lease=%x", leaseID)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed descriptor did not compensate its completed remote lease")
	}
	if _, err := receiver.RequestLane(context.Background(), 0); err != nil {
		t.Fatalf("descriptor compensation damaged sibling work: %v", err)
	}
}

func TestContentClientRejectsMissingAndZeroLeaseAuthorityLocally(t *testing.T) {
	var nilClient *receiverRevisionClient
	if _, _, _, err := nilClient.beginExternalOperation(context.Background()); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil revision client admission error=%v", err)
	}
	nilClient.stop()
	nilClient.close()

	lane := &receiverBlockLane{
		revisions: &receiverRevisionClient{leases: make(map[content.LeaseID]*remoteLeaseState)},
	}
	if _, err := lane.FetchBlock(context.Background(), transfer.BlockDemand{
		LeaseID: id16[content.LeaseID](141),
	}); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("unknown local lease demand error=%v", err)
	}

	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	if err := receiver.revisions.releaseRemoteLease(context.Background(), content.LeaseID{}); err == nil {
		t.Fatal("zero release capability reached RPC admission")
	}
	if _, err := receiver.revisions.renew(context.Background(), content.LeaseID{}); err == nil {
		t.Fatal("zero renew capability reached RPC admission")
	}
}

func TestRemoteLeaseFailureRemainsOperationScoped(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	unknownLease := id16[content.LeaseID](142)
	if _, err := receiver.revisions.renew(context.Background(), unknownLease); err == nil {
		t.Fatal("unknown remote lease renewed")
	} else {
		var remote RemoteOperationError
		if !errors.As(err, &remote) || remote.Failure().Scope() != protocolsession.OperationScopeRevision {
			t.Fatalf("unknown renew failure=%#v error=%v", remote, err)
		}
	}

	if _, err := receiver.RequestLane(context.Background(), 0); err != nil {
		t.Fatalf("operation-scoped lease failure closed the session: %v", err)
	}
}
