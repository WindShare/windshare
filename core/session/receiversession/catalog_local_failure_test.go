package receiversession

import (
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/windshare/windshare/core/catalog"

	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	"testing"
	"testing/synctest"
	"time"
)

type blockingCatalogPageSource struct {
	started chan context.Context
	stopped chan struct{}
}

var _ catalogflow.PageAddressedSource = (*blockingCatalogPageSource)(nil)

func (source *blockingCatalogPageSource) LoadPage(
	ctx context.Context,
	_ catalogflow.ListRequest,
	_ catalog.ScanProgressObserver,
) (catalogflow.PageResult, error) {
	select {
	case source.started <- ctx:
	case <-ctx.Done():
		return catalogflow.PageResult{}, ctx.Err()
	}
	<-ctx.Done()
	select {
	case source.stopped <- struct{}{}:
	default:
	}
	return catalogflow.PageResult{}, ctx.Err()
}

func TestCatalogLocalFailuresCancelExactOperationAndPreserveSibling(t *testing.T) {
	observerFailure := errors.New("catalog progress consumer stopped")
	tests := []struct {
		name                         string
		observer                     CatalogScanProgressObserver
		want                         error
		progress                     func(*testing.T) [][]byte
		bypassSemanticAuthentication bool
	}{
		{
			name: "progress observer",
			observer: CatalogScanProgressObserverFunc(func(context.Context, CatalogScanProgress) error {
				return observerFailure
			}),
			want: observerFailure,
			progress: func(t *testing.T) [][]byte {
				return [][]byte{encodeCatalogProgress(t, id16[catalog.ScanAttemptID](101), 1)}
			},
		},
		{
			name:                         "progress decode",
			want:                         ErrScanProgress,
			bypassSemanticAuthentication: true,
			progress: func(*testing.T) [][]byte {
				return [][]byte{{1}}
			},
		},
		{
			name: "progress state",
			want: ErrScanProgress,
			progress: func(t *testing.T) [][]byte {
				attempt := id16[catalog.ScanAttemptID](102)
				return [][]byte{
					encodeCatalogProgress(t, attempt, 2),
					encodeCatalogProgress(t, attempt, 1),
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newVerticalFixture(t)
				source := &blockingCatalogPageSource{
					started: make(chan context.Context, 1), stopped: make(chan struct{}, 2),
				}
				service, err := catalogflow.NewAddressedSenderService(fixture.share, source)
				if err != nil {
					t.Fatal(err)
				}
				senderFactory := newLocalFailureSenderFactory(
					t, fixture,
					sendersession.CatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
						return service, nil
					}),
					nil,
				)
				receiverFactory := newLocalFailureReceiverFactory(t, fixture, func(config *FactoryConfig) {
					config.CatalogProgress = test.observer
				})
				pair := connectAuditedVerticalPair(t, senderFactory, receiverFactory)
				receiverTombstones := 0
				senderTombstones := 0
				result := make(chan error, 1)
				go func() {
					_, err := pair.receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
					result <- err
				}()
				var senderContext context.Context
				select {
				case senderContext = <-source.started:
				case <-time.After(time.Second):
					t.Fatal("sender catalog handler did not reach the blocked page source")
				}
				operation := captureExactOperationLifecycle(
					t, pair, senderContext, receiverTombstones, senderTombstones,
					protocolsession.MessageListChildren,
				)
				pair.beginReceiverFrameAudit()
				for _, body := range test.progress(t) {
					if test.bypassSemanticAuthentication {
						// Malformed signed CBOR is normally rejected before routing. This
						// white-box admission isolates the transport decoder's local-failure
						// cleanup while the request itself still enters through the public API.
						deliverInjectedReceiverMessage(t, pair.receiver, signedPeerOperationControl(
							t, pair.receiver, fixture.senderConfig.SenderPrivateKey,
							protocolsession.MessageScanProgress, operation.call.ID(), body,
						))
						continue
					}
					sendInjectedCatalogProgress(t, pair.receiver, fixture.senderConfig.SenderPrivateKey, operation.call.ID(), body)
				}
				select {
				case err := <-result:
					if !errors.Is(err, test.want) {
						t.Fatalf("catalog local failure=%v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("catalog local failure did not finish its public caller")
				}
				select {
				case <-source.stopped:
				case <-time.After(time.Second):
					t.Fatal("catalog local failure did not cancel the exact remote page load")
				}
				assertExactOperationDrained(t, pair, operation)
				select {
				case <-source.stopped:
					t.Fatal("catalog local failure canceled the remote page load more than once")
				default:
				}
				if pair.receiver.Err() != nil || pair.sender.Err() != nil {
					t.Fatalf("operation-local catalog failure terminated session: receiver=%v sender=%v",
						pair.receiver.Err(), pair.sender.Err())
				}
				if _, err := pair.receiver.RequestLane(context.Background(), 0); err != nil {
					t.Fatalf("catalog failure damaged sibling operation: %v", err)
				}
			})
		})
	}
}

func encodeCatalogProgress(
	t *testing.T,
	attempt catalog.ScanAttemptID,
	discovered uint64,
) []byte {
	t.Helper()
	body, err := protocolsession.EncodeScanProgress(protocolsession.ScanProgress{
		AttemptID: attempt, DiscoveredEntries: discovered,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func sendInjectedCatalogProgress(
	t *testing.T,
	receiver *Runtime,
	privateKey ed25519.PrivateKey,
	operationID protocolsession.OperationID,
	body []byte,
) {
	t.Helper()
	message := signedPeerOperationControl(t, receiver, privateKey, protocolsession.MessageScanProgress, operationID, body)
	deliverInjectedReceiverMessage(t, receiver, message)
}

func deliverInjectedReceiverMessage(t *testing.T, receiver *Runtime, message protocolsession.Message) {
	t.Helper()
	call := onlyActiveCall(t, receiver.rpc)
	ctx := receiverRequestContext(call)
	if err := receiver.rpc.HandleMessage(ctx, message); err != nil {
		t.Fatalf("enqueue authenticated receiver message: %v", err)
	}
}
