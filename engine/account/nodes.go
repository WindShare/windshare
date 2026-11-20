package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
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

// nodePolicyCache owns relay node descriptor discovery and anonymous-admission
// caching independently of account credential state.
// Lock ordering with Service: Service.mu -> nodePolicyCache.mu.
type nodePolicyCache struct {
	ctx         context.Context
	workers     *sync.WaitGroup
	client      HTTPClient
	now         func() time.Time
	resolveNode func(string) (Node, bool)
	onChange    func()
	mu          sync.Mutex
	entries     map[string]*nodeState
}

func newNodePolicyCache(
	ctx context.Context,
	workers *sync.WaitGroup,
	client HTTPClient,
	now func() time.Time,
	resolveNode func(string) (Node, bool),
	onChange func(),
) *nodePolicyCache {
	return &nodePolicyCache{
		ctx:         ctx,
		workers:     workers,
		client:      client,
		now:         now,
		resolveNode: resolveNode,
		onChange:    onChange,
		entries:     make(map[string]*nodeState),
	}
}

func (cache *nodePolicyCache) retain(address string) {
	if cache.ctx.Err() != nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	state := cache.entries[address]
	if state == nil {
		state = &nodeState{}
		cache.entries[address] = state
	}
	state.refs++
	if state.cancel != nil {
		return
	}
	nodeCtx, cancel := context.WithCancel(cache.ctx)
	state.cancel = cancel
	cache.workers.Go(func() {
		cache.pollNode(nodeCtx, address, state)
	})
}

func (cache *nodePolicyCache) release(address string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	state := cache.entries[address]
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
	delete(cache.entries, address)
}

func (cache *nodePolicyCache) permitsAnonymous(address string, node Node) bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cached := cache.entries[address]
	return cached != nil && cached.key == node.Key &&
		cached.expires.After(cache.now()) && cached.anonymous && cached.available
}

func (cache *nodePolicyCache) put(address string, state *nodeState) {
	cache.mu.Lock()
	cache.entries[address] = state
	cache.mu.Unlock()
}

func (cache *nodePolicyCache) len() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.entries)
}

func (cache *nodePolicyCache) pollNode(ctx context.Context, address string, state *nodeState) {
	for {
		cache.refreshNode(ctx, address, state)
		timer := time.NewTimer(RefreshInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (cache *nodePolicyCache) refreshNode(ctx context.Context, address string, state *nodeState) {
	node, official := cache.resolveNode(address)
	if !official {
		return
	}
	descriptor, err := cache.discover(ctx, address, node.Key)
	if err != nil {
		return
	}
	updated := false
	cache.mu.Lock()
	if cache.entries[address] == state && ctx.Err() == nil {
		state.key = node.Key
		state.expires = time.Unix(int64(descriptor.ExpiresAt), 0)
		state.anonymous = descriptor.Anonymous
		state.available = descriptor.Available && descriptor.Services.Relay
		updated = true
	}
	cache.mu.Unlock()
	// Notify after releasing cache.mu to preserve Service.mu -> nodePolicyCache.mu order.
	if updated && cache.onChange != nil {
		cache.onChange()
	}
}

func (cache *nodePolicyCache) discover(ctx context.Context, address string, key [32]byte) (serviceauthority.Descriptor, error) {
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
	response, err := cache.client.Do(request)
	if err != nil {
		return serviceauthority.Descriptor{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, serviceauthority.MaximumDescriptorBytes+1))
	if err != nil || response.StatusCode != http.StatusOK {
		return serviceauthority.Descriptor{}, errors.Join(serviceauthority.ErrFormat, err)
	}
	descriptor, err := serviceauthority.VerifyDescriptor(data, key, cache.now())
	if err != nil {
		return descriptor, err
	}
	if descriptor.RelayEndpoint != endpoint.IdentityURL {
		return serviceauthority.Descriptor{}, serviceauthority.ErrSignature
	}
	return descriptor, nil
}
