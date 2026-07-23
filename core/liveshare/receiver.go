package liveshare

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

const outputResumeIntentDomain = "windshare/v2 output-resume-intent\x00"

var errReceiverClosed = errors.New("live share receiver is closed")

type ReceiverConfig struct {
	Capability       link.Link
	DescriptorObject []byte
	Random           io.Reader
	CatalogProgress  sessionruntime.CatalogScanProgressObserver
	PeerControls     sessionruntime.ReceiverPeerSemantics
}

type PreparedReceiver struct {
	mu sync.Mutex

	descriptor   catalog.ShareDescriptor
	factory      *sessionruntime.ReceiverFactory
	resources    *receiverRuntimeResources
	resumeIntent osfs.OutputResumeIntent
	closed       bool
	closeDone    chan struct{}
}

func PrepareReceiver(config ReceiverConfig) (*PreparedReceiver, error) {
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
	descriptor, err := catalogflow.OpenDescriptor(
		config.DescriptorObject, capability.PKHash, shareIDRaw, descriptorKey,
	)
	clear(descriptorKey)
	if err != nil {
		return nil, err
	}
	publicKey := ed25519.PublicKey(descriptor.SenderPublicKey())
	keyTree, err := content.NewKeyTree(capability.ReadSecret, descriptor.ShareInstance())
	if err != nil {
		return nil, err
	}
	var resources *receiverRuntimeResources
	fail := func(cause error) (*PreparedReceiver, error) {
		if resources == nil {
			keyTree.Destroy()
		} else {
			resources.Close()
		}
		return nil, cause
	}
	catalogKey, err := keyTree.CatalogKey()
	if err != nil {
		return fail(err)
	}
	verifier, err := catalogflow.NewCatalogObjectVerifier(catalogflow.CatalogObjectVerifierConfig{
		ShareInstance: descriptor.ShareInstance(), CatalogKey: catalogKey.Bytes(), SenderPublicKey: publicKey,
	})
	catalogKey.Destroy()
	if err != nil {
		return fail(err)
	}
	resources = newReceiverRuntimeResources(keyTree, verifier)
	opener, err := records.NewOpener(records.OpenerConfig{
		ShareInstance: descriptor.ShareInstance(), Keys: keyTree, VerificationKey: publicKey,
	})
	if err != nil {
		return fail(err)
	}
	processReassembly, err := contentflow.NewReassemblyAccount("live-get-process", contentflow.ReassemblyLimits{
		Bytes: contentflow.DefaultProcessReassemblyBytes, Records: contentflow.DefaultProcessReassemblyRecords,
	})
	if err != nil {
		return fail(err)
	}
	shareReassembly, err := contentflow.NewReassemblyAccount("live-get-share", contentflow.ReassemblyLimits{
		Bytes: contentflow.DefaultShareReassemblyBytes, Records: contentflow.DefaultShareReassemblyRecords,
	})
	if err != nil {
		return fail(err)
	}
	plaintext, err := transfer.NewPlaintextBudget(transfer.DefaultProcessPlaintextBytes)
	if err != nil {
		return fail(err)
	}
	sessionAuthKey, err := keyTree.SessionAuthKey()
	if err != nil {
		return fail(err)
	}
	authKey := sessionAuthKey.Bytes()
	sessionAuthKey.Destroy()
	digest := sha256.New()
	_, _ = digest.Write([]byte(outputResumeIntentDomain))
	_, _ = digest.Write(capability.ReadSecret)
	_, _ = digest.Write(descriptor.ShareInstance().Bytes())
	resumeIntent, err := osfs.OutputResumeIntentFromBytes(digest.Sum(nil))
	if err != nil {
		clear(authKey)
		return fail(err)
	}
	factory, err := sessionruntime.NewReceiverFactory(sessionruntime.ReceiverFactoryConfig{
		Descriptor: descriptor, SessionAuthKey: authKey, SenderPublicKey: publicKey,
		CatalogVerifier: verifier, RecordOpener: opener,
		ReassemblyProcess: processReassembly, ReassemblyShare: shareReassembly, PlaintextProcess: plaintext,
		Random: config.Random, CatalogProgress: config.CatalogProgress, PeerControls: config.PeerControls,
		RuntimeResources: resources,
	})
	if err != nil {
		clear(authKey)
		return fail(err)
	}
	// The factory owns the only post-construction copy; retaining a facade alias
	// would expand the secret lifetime without serving any receiver operation.
	clear(authKey)
	return &PreparedReceiver{
		descriptor: descriptor, factory: factory, resources: resources, resumeIntent: resumeIntent,
		closeDone: make(chan struct{}),
	}, nil
}

func (receiver *PreparedReceiver) Descriptor() catalog.ShareDescriptor {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.descriptor
}

func (receiver *PreparedReceiver) Connect(ctx context.Context, channel framechannel.Channel) (*sessionruntime.ReceiverRuntime, error) {
	receiver.mu.Lock()
	if receiver.closed {
		receiver.mu.Unlock()
		return nil, errReceiverClosed
	}
	factory := receiver.factory
	receiver.mu.Unlock()
	return factory.Connect(ctx, channel)
}

func (receiver *PreparedReceiver) OpenOutput(ctx context.Context, rootPath string) (osfs.FilesystemOutputOpen, error) {
	receiver.mu.Lock()
	if receiver.closed {
		receiver.mu.Unlock()
		return osfs.FilesystemOutputOpen{}, errReceiverClosed
	}
	share, intent := receiver.descriptor.ShareInstance(), receiver.resumeIntent
	receiver.mu.Unlock()
	authority, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{})
	if err != nil {
		return osfs.FilesystemOutputOpen{}, err
	}
	return authority.OpenOrCreate(ctx, osfs.FilesystemOutputIntent{
		RootPath: rootPath, ShareInstance: share, ResumeIntent: intent,
	})
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
	done, factory, resources := receiver.closeDone, receiver.factory, receiver.resources
	if factory != nil {
		// Factory initiation is callback-free. Keeping it inside this transition
		// lock makes every concurrent BeginClose return imply frozen handshakes.
		factory.BeginClose()
	}
	receiver.mu.Unlock()
	go receiver.finishClose(factory, resources, done)
}

func (receiver *PreparedReceiver) finishClose(
	factory *sessionruntime.ReceiverFactory,
	resources *receiverRuntimeResources,
	done chan struct{},
) {
	if factory != nil {
		factory.WaitClosed()
	}
	receiver.mu.Lock()
	receiver.factory = nil
	receiver.mu.Unlock()
	if resources != nil {
		resources.Close()
	}
	receiver.mu.Lock()
	receiver.resources = nil
	receiver.resumeIntent = osfs.OutputResumeIntent{}
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

// receiverRuntimeResources delays secret destruction until every runtime that
// can still authenticate content has released its lease. Closing the facade
// still closes admission immediately; it must not invalidate an in-flight job.
type receiverRuntimeResources struct {
	mu       sync.Mutex
	keyTree  *content.KeyTree
	verifier *catalogflow.CatalogObjectVerifier
	active   int
	closed   bool
}

func newReceiverRuntimeResources(
	keyTree *content.KeyTree,
	verifier *catalogflow.CatalogObjectVerifier,
) *receiverRuntimeResources {
	return &receiverRuntimeResources{keyTree: keyTree, verifier: verifier}
}

func (resources *receiverRuntimeResources) AcquireReceiverRuntimeResources() (
	sessionruntime.ReceiverRuntimeResourceLease,
	error,
) {
	resources.mu.Lock()
	defer resources.mu.Unlock()
	if resources.closed {
		return nil, errReceiverClosed
	}
	resources.active++
	return &receiverRuntimeResourceLease{resources: resources}, nil
}

func (resources *receiverRuntimeResources) Close() {
	if resources == nil {
		return
	}
	resources.mu.Lock()
	resources.closed = true
	keyTree, verifier := resources.takeSecretsIfIdle()
	resources.mu.Unlock()
	destroyReceiverSecrets(keyTree, verifier)
}

func (resources *receiverRuntimeResources) release() {
	resources.mu.Lock()
	resources.active--
	keyTree, verifier := resources.takeSecretsIfIdle()
	resources.mu.Unlock()
	destroyReceiverSecrets(keyTree, verifier)
}

func (resources *receiverRuntimeResources) takeSecretsIfIdle() (
	*content.KeyTree,
	*catalogflow.CatalogObjectVerifier,
) {
	if !resources.closed || resources.active != 0 {
		return nil, nil
	}
	keyTree, verifier := resources.keyTree, resources.verifier
	resources.keyTree = nil
	resources.verifier = nil
	return keyTree, verifier
}

func destroyReceiverSecrets(keyTree *content.KeyTree, verifier *catalogflow.CatalogObjectVerifier) {
	if verifier != nil {
		verifier.Destroy()
	}
	if keyTree != nil {
		keyTree.Destroy()
	}
}

type receiverRuntimeResourceLease struct {
	resources *receiverRuntimeResources
	once      sync.Once
}

func (lease *receiverRuntimeResourceLease) Release() {
	if lease == nil {
		return
	}
	lease.once.Do(lease.resources.release)
}
