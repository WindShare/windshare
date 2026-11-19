package liveshare

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"

	sessionobs "github.com/windshare/windshare/core/session/observation"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

var errReceiverClosed = errors.New("live share receiver is closed")

type ReceiverConfig struct {
	Capacity                          *ReceiveCapacity
	ContentRoutePolicy                transferlanes.ContentRoutePolicy
	Capability                        link.Link
	DescriptorObject                  []byte
	Random                            io.Reader
	CatalogProgress                   receiversession.CatalogScanProgressObserver
	PeerControls                      receiversession.PeerSemantics
	ProtocolObservations              observationstream.Producer[sessionobs.ProtocolObservation]
	LaneSettlementObservationCapacity transferlanes.SettlementObservationCapacity
}

type PreparedReceiver struct {
	mu sync.Mutex

	descriptor catalog.ShareDescriptor
	factory    *receiversession.Factory
	closed     bool
	closeDone  chan struct{}
}

func PrepareReceiver(config ReceiverConfig) (*PreparedReceiver, error) {
	if config.Capacity == nil {
		return nil, errors.New("live share receiver requires application receive capacity")
	}
	capability := config.Capability
	if capability.Suite != link.SuiteSenderAuthenticated || len(capability.ReadSecret) != link.ReadSecretBytes ||
		len(capability.PKHash) != link.PKHashBytes || len(capability.Relays) == 0 || len(config.DescriptorObject) == 0 {
		return nil, errors.New("live share receiver requires a complete suite-02 capability and descriptor")
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	shareIDRaw, err := base64.RawURLEncoding.Strict().DecodeString(capability.ShareID)
	if err != nil || len(shareIDRaw) != link.SenderAuthenticatedShareIDBytes {
		return nil, errors.Join(link.ErrMalformedShareID, err)
	}
	descriptorKey, err := keyderiv.V2DescriptorKey(capability.ReadSecret, capability.PKHash)
	if err != nil {
		return nil, err
	}
	opened, err := catalogflow.OpenDescriptor(
		config.DescriptorObject, capability.PKHash, shareIDRaw, descriptorKey,
	)
	clear(descriptorKey)
	if err != nil {
		return nil, err
	}
	descriptor, sender := opened.Descriptor, opened.Sender
	keyTree, err := content.NewKeyTree(capability.ReadSecret, descriptor.ShareInstance())
	if err != nil {
		return nil, err
	}
	defer keyTree.Destroy()
	shareReassembly, err := contentflow.NewReassemblyAccount("live-get-share", contentflow.ReassemblyLimits{
		Bytes: contentflow.DefaultShareReassemblyBytes, Records: contentflow.DefaultShareReassemblyRecords,
	})
	if err != nil {
		return nil, err
	}
	sessionAuthKey, err := keyTree.SessionAuthKey()
	if err != nil {
		return nil, err
	}
	authKey := sessionAuthKey.Bytes()
	sessionAuthKey.Destroy()
	factory, err := receiversession.NewFactory(receiversession.FactoryConfig{
		ContentRoutePolicy: config.ContentRoutePolicy,
		Descriptor:         descriptor, SessionAuthKey: authKey, Sender: sender,
		CatalogVerifier: catalogflow.CatalogPayloadDecoder{}, RecordDecoder: records.Decoder{ShareInstance: descriptor.ShareInstance()},
		ReassemblyProcess: config.Capacity.reassembly, ReassemblyShare: shareReassembly, PlaintextProcess: config.Capacity.plaintext,
		Random: config.Random, CatalogProgress: config.CatalogProgress, PeerControls: config.PeerControls,
		ProtocolObservations:              config.ProtocolObservations,
		LaneSettlementObservationCapacity: config.LaneSettlementObservationCapacity,
	})
	if err != nil {
		clear(authKey)
		return nil, err
	}
	// The factory owns the only post-construction copy; retaining a facade alias
	// would expand the secret lifetime without serving any receiver operation.
	clear(authKey)
	return &PreparedReceiver{
		descriptor: descriptor, factory: factory,
		closeDone: make(chan struct{}),
	}, nil
}

func (receiver *PreparedReceiver) Descriptor() catalog.ShareDescriptor {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.descriptor
}

func (receiver *PreparedReceiver) Connect(ctx context.Context, channel framechannel.Channel, route transferlanes.Route) (*receiversession.Runtime, error) {
	receiver.mu.Lock()
	if receiver.closed {
		receiver.mu.Unlock()
		return nil, errReceiverClosed
	}
	factory := receiver.factory
	receiver.mu.Unlock()
	return factory.Connect(ctx, channel, route)
}

// Close is the owner-side join. Dependency callbacks must call BeginClose so
// they do not wait for the admission whose cleanup invoked them.
func (receiver *PreparedReceiver) Close() {
	if receiver == nil {
		return
	}
	receiver.BeginClose()
	receiver.WaitClosed()
}

// BeginClose freezes both facade and handshake admission before returning, then
// delegates blocking joins to a worker so dependency callbacks can reenter it.
func (receiver *PreparedReceiver) BeginClose() {
	if receiver == nil {
		return
	}
	receiver.mu.Lock()
	if receiver.closed {
		receiver.mu.Unlock()
		return
	}
	receiver.closed = true
	if receiver.closeDone == nil {
		receiver.closeDone = make(chan struct{})
	}
	done, factory := receiver.closeDone, receiver.factory
	if factory != nil {
		// Factory initiation is callback-free. Keeping it inside this transition
		// lock makes every concurrent BeginClose return imply frozen handshakes.
		factory.BeginClose()
	}
	receiver.mu.Unlock()
	go receiver.finishClose(factory, done)
}

func (receiver *PreparedReceiver) finishClose(
	factory *receiversession.Factory,
	done chan struct{},
) {
	if factory != nil {
		factory.WaitClosed()
	}
	receiver.mu.Lock()
	receiver.factory = nil
	close(done)
	receiver.mu.Unlock()
}

func (receiver *PreparedReceiver) WaitClosed() {
	if receiver == nil {
		return
	}
	receiver.mu.Lock()
	done := receiver.closeDone
	receiver.mu.Unlock()
	if done != nil {
		<-done
	}
}
