package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type nodeState struct {
	key       [32]byte
	expires   time.Time
	anonymous bool
	available bool
	refs      int
	cancel    context.CancelFunc
}

func (service *Service) retainNodeLocked(address string) {
	if service.ctx.Err() != nil {
		return
	}
	state := service.nodes[address]
	if state == nil {
		state = &nodeState{}
		service.nodes[address] = state
	}
	state.refs++
	if state.cancel != nil {
		return
	}
	nodeCtx, cancel := context.WithCancel(service.ctx)
	state.cancel = cancel
	service.workers.Go(func() {
		service.pollNode(nodeCtx, address, state)
	})
}

func (service *Service) releaseNodeLocked(address string) {
	state := service.nodes[address]
	if state == nil {
		return
	}
	if state.refs > 0 {
		state.refs--
	}
	if state.refs > 0 {
		return
	}
	if state.cancel != nil {
		state.cancel()
		state.cancel = nil
	}
	delete(service.nodes, address)
}

func (service *Service) pollNode(ctx context.Context, address string, state *nodeState) {
	for {
		service.refreshNode(ctx, address, state)
		timer := time.NewTimer(RefreshInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (service *Service) refreshNode(ctx context.Context, address string, state *nodeState) {
	if service.config.ResolveNode == nil {
		return
	}
	node, official := service.config.ResolveNode(address)
	if !official {
		return
	}
	descriptor, err := service.discover(ctx, address, node.Key)
	if err != nil {
		return
	}
	service.mu.Lock()
	if service.nodes[address] == state && ctx.Err() == nil {
		state.key = node.Key
		state.expires = time.Unix(int64(descriptor.ExpiresAt), 0)
		state.anonymous = descriptor.Anonymous
		state.available = descriptor.Available && descriptor.Services.Relay
		service.notifyLocked()
	}
	service.mu.Unlock()
}

func (service *Service) discover(ctx context.Context, address string, key [32]byte) (serviceauthority.Descriptor, error) {
	endpoint, err := v2.NormalizeRelayEndpoint(address)
	if err != nil {
		return serviceauthority.Descriptor{}, err
	}
	uri, err := url.Parse(endpoint.DialURL)
	if err != nil {
		return serviceauthority.Descriptor{}, err
	}
	switch uri.Scheme {
	case "wss":
		uri.Scheme = "https"
	case "ws":
		uri.Scheme = "http"
	}
	uri.Path, uri.RawPath, uri.RawQuery, uri.Fragment = "/.well-known/windshare", "", "", ""
	requestContext, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, uri.String(), nil)
	if err != nil {
		return serviceauthority.Descriptor{}, err
	}
	response, err := service.config.Client.Do(request)
	if err != nil {
		return serviceauthority.Descriptor{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, serviceauthority.MaximumDescriptorBytes+1))
	if err != nil || response.StatusCode != http.StatusOK {
		return serviceauthority.Descriptor{}, errors.Join(serviceauthority.ErrFormat, err)
	}
	descriptor, err := serviceauthority.VerifyDescriptor(data, key, service.config.Now())
	if err != nil {
		return descriptor, err
	}
	if descriptor.RelayEndpoint != endpoint.IdentityURL {
		return serviceauthority.Descriptor{}, serviceauthority.ErrSignature
	}
	return descriptor, nil
}
