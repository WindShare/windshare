package cli

import "github.com/windshare/windshare/connectivity/v2peer"

func (a *App) newSenderPeerFactory() (*v2peer.Factory, error) {
	return v2peer.NewFactory(v2peer.Config{
		Configuration: v2peer.DefaultConfiguration(),
		OnError: func(error) {
			a.logf("share: direct peer lane failed; relay service remains available")
		},
	})
}
