package cli

import "github.com/windshare/windshare/connectivity/v2peer"

type SenderPeerFactoryOptions struct {
	Observer v2peer.SenderAttemptObserver
	OnError  func(error)
}

// SenderPeerFactoryProvider is defined at the CLI consumer boundary so a test
// entry can own both Pion API construction and RTCConfiguration. Returning the
// concrete factory prevents a second policy layer from silently reapplying the
// production STUN configuration.
type SenderPeerFactoryProvider interface {
	NewSenderPeerFactory(SenderPeerFactoryOptions) (*v2peer.Factory, error)
}

type SenderPeerFactoryProviderFunc func(SenderPeerFactoryOptions) (*v2peer.Factory, error)

func (function SenderPeerFactoryProviderFunc) NewSenderPeerFactory(
	options SenderPeerFactoryOptions,
) (*v2peer.Factory, error) {
	if function == nil {
		return nil, v2peer.ErrConfig
	}
	return function(options)
}

type productionSenderPeerFactoryProvider struct{}

func (productionSenderPeerFactoryProvider) NewSenderPeerFactory(
	options SenderPeerFactoryOptions,
) (*v2peer.Factory, error) {
	return v2peer.NewFactory(v2peer.Config{
		Configuration: v2peer.DefaultConfiguration(),
		Observer:      options.Observer,
		OnError:       options.OnError,
	})
}

func (a *App) newSenderPeerFactory() (*v2peer.Factory, error) {
	provider := a.senderPeerFactories
	if provider == nil {
		provider = productionSenderPeerFactoryProvider{}
	}
	return provider.NewSenderPeerFactory(SenderPeerFactoryOptions{
		Observer: newSenderEvidenceProjector(a.senderPeerEvidence, func(err error) {
			a.logf("share: project direct peer evidence: %v", err)
		}),
		OnError: func(error) {
			a.logf("share: direct peer lane failed; relay service remains available")
		},
	})
}
