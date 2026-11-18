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
}

func (service *Service) startNode(address string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.ctx.Err() != nil || service.nodes[address] != nil {
		return
	}
	service.nodes[address] = &nodeState{}
	service.workers.Go(func() {
		for {
			if service.config.ResolveNode != nil {
				if node, official := service.config.ResolveNode(address); official {
					if descriptor, err := service.discover(service.ctx, address, node.Key); err == nil {
						service.mu.Lock()
						service.nodes[address] = &nodeState{key: node.Key, expires: time.Unix(int64(descriptor.ExpiresAt), 0),
							anonymous: descriptor.Anonymous, available: descriptor.Available && descriptor.Services.Relay}
						service.notifyLocked()
						service.mu.Unlock()
					}
				}
			}
			timer := time.NewTimer(RefreshInterval)
			select {
			case <-service.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	})
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
