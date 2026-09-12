package contentflow

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type invalidLeaseTraceStore struct{ RevisionStore }

func (invalidLeaseTraceStore) ValidateLease(content.LeaseID, content.FileRevisionDescriptor) error {
	return content.ErrInvalidLease
}

func TestBlockLeaseRejectionKeepsOwnershipDecisionAndWireBehavior(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		stage SenderDecisionStage
		code  uint16
	}{
		{"released", SenderDecisionBlockLeaseReleased, RevisionCodeInvalidLease},
		{"not_owned", SenderDecisionBlockLeaseNotOwned, RevisionCodeInvalidLease},
		{"expired", SenderDecisionBlockLeaseExpired, RevisionCodeLeaseExpired},
		{"invalid", SenderDecisionBlockLeaseInvalid, RevisionCodeInvalidLease},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newRuntimeFixture(t, 1)
			defer fixture.close(t)
			results, err := fixture.service.Open(context.Background(), mustOpenRequest(t, fixture.file))
			if err != nil {
				t.Fatal(err)
			}
			lease := results.Items()[0].Lease
			leaseID := lease.ID()
			outbound := newRecordingOutbound()
			var decisions []SenderDecisionTrace
			handler, err := NewSenderHandler(SenderHandlerConfig{
				Service: fixture.service, Outbound: outbound,
				DecisionTracer: SenderDecisionTraceFunc(func(event SenderDecisionTrace) { decisions = append(decisions, event) }),
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := NewBlockRequest(leaseID, []uint64{0})
			if err != nil {
				t.Fatal(err)
			}
			body, err := EncodeBlockRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			operation := protocolsession.OperationID{81}
			handler.process(context.Background(), operationMessage(t, protocolsession.MessageRequestBlocks, operation, body))
			if len(decisions) != 0 || len(outbound.controls) != 1 {
				t.Fatal("successful blocks must stay off the rejection trace")
			}
			switch scenario.name {
			case "released":
				releaseBody, encodeErr := EncodeLeaseRequest(leaseID)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				handler.process(context.Background(), operationMessage(t, protocolsession.MessageReleaseLease,
					protocolsession.OperationID{82}, releaseBody))
				if len(decisions) != 1 || decisions[0].Stage != SenderDecisionLeaseRelinquished ||
					decisions[0].LeaseID != leaseID {
					t.Fatal("release lost its lease join key")
				}
			case "not_owned":
				leaseID = content.LeaseID{0xff}
			case "expired":
				fixture.clock.Advance(lease.TTL() + time.Second)
			case "invalid":
				fixture.service.store = invalidLeaseTraceStore{RevisionStore: fixture.service.store}
			}
			request, err = NewBlockRequest(leaseID, []uint64{0})
			if err != nil {
				t.Fatal(err)
			}
			body, err = EncodeBlockRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			operation = protocolsession.OperationID{83}
			handler.process(context.Background(), operationMessage(t, protocolsession.MessageRequestBlocks, operation, body))
			select {
			case failure := <-outbound.failures:
				if failure.Code != scenario.code || failure.Scope != RevisionErrorScope {
					t.Fatalf("wire failure = %+v", failure)
				}
			default:
				t.Fatal("rejected lease had no operation failure")
			}
			got := decisions[len(decisions)-1]
			if got.Stage != scenario.stage || got.OperationID != operation || got.LeaseID != leaseID ||
				got.RequestKind != protocolsession.MessageRequestBlocks {
				t.Fatalf("rejection decision = %+v", got)
			}
		})
	}
}
