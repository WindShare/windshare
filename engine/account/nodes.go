package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type nodeState struct {
	key       [32]byte
	expires   time.Time
	anonymous bool
	available bool
}

type nodePoller struct {
	refs   int
	cancel context.CancelFunc
}

// nodePolicyCache owns relay node descriptor discovery and anonymous-admission
// caching independently of account credential state. Active polling follows
// pending-attribution demand, while verified signed policies remain reusable
// until signature expiry or directory key rotation.
// Lock ordering with Service: Service.mu -> nodePolicyCache.mu.
type nodePolicyCache struct {
	ctx         context.Context
	workers     *sync.WaitGroup
	client      HTTPClient
	now         func() time.Time
	resolveNode func(string) (Node, bool)
	onChange    func()
	diagnostics diagnostics.Sink
	mu          sync.Mutex
	policies    map[string]nodeState
	pollers     map[string]*nodePoller
}

func newNodePolicyCache(
	ctx context.Context,
	workers *sync.WaitGroup,
	client HTTPClient,
	now func() time.Time,
	resolveNode func(string) (Node, bool),
	onChange func(),
	sink diagnostics.Sink,
) *nodePolicyCache {
	return &nodePolicyCache{
		ctx:         ctx,
		workers:     workers,
		client:      client,
		now:         now,
		resolveNode: resolveNode,
		onChange:    onChange,
		diagnostics: sink,
		policies:    make(map[string]nodeState),
		pollers:     make(map[string]*nodePoller),
	}
}

func (cache *nodePolicyCache) retain(address string) {
	if cache.ctx.Err() != nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if poller := cache.pollers[address]; poller != nil {
		poller.refs++
		return
	}
	nodeCtx, cancel := context.WithCancel(cache.ctx)
	poller := &nodePoller{refs: 1, cancel: cancel}
	cache.pollers[address] = poller
	cache.workers.Go(func() {
		cache.pollNode(nodeCtx, address, poller)
	})
}

func (cache *nodePolicyCache) release(address string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	poller := cache.pollers[address]
	if poller == nil {
		return
	}
	if poller.refs > 0 {
		poller.refs--
	}
	if poller.refs > 0 {
		return
	}
	poller.cancel()
	delete(cache.pollers, address)
	cache.pruneExpiredPoliciesLocked(cache.now())
}

func (state nodeState) permits(key [32]byte, now time.Time) bool {
	return state.key == key && state.expires.After(now) && state.anonymous && state.available
}

func (cache *nodePolicyCache) permitsAnonymous(address string, node Node) bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	policy, ok := cache.policies[address]
	if !ok {
		return false
	}
	if policy.permits(node.Key, cache.now()) {
		return true
	}
	// Evict expired, key-mismatched, or non-permitting entries on read so
	// callers immediately rediscover against the node's current directory key.
	delete(cache.policies, address)
	return false
}

func (cache *nodePolicyCache) pruneExpiredPoliciesLocked(now time.Time) {
	for address, policy := range cache.policies {
		if !policy.expires.After(now) {
			delete(cache.policies, address)
		}
	}
}

func (cache *nodePolicyCache) put(address string, state *nodeState) {
	cache.mu.Lock()
	if state == nil {
		delete(cache.policies, address)
	} else {
		cache.policies[address] = *state
	}
	cache.mu.Unlock()
}

func (cache *nodePolicyCache) pollerCount() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.pollers)
}

func (cache *nodePolicyCache) policyCount() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.policies)
}

func (cache *nodePolicyCache) pollNode(ctx context.Context, address string, poller *nodePoller) {
	for {
		cache.refreshNode(ctx, address, poller)
		timer := time.NewTimer(RefreshInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (cache *nodePolicyCache) refreshNode(ctx context.Context, address string, poller *nodePoller) {
	node, official := cache.resolveNode(address)
	if !official {
		cache.mu.Lock()
		if cache.pollers[address] == poller && ctx.Err() == nil {
			delete(cache.policies, address)
		}
		cache.mu.Unlock()
		return
	}
	descriptor, err := cache.discover(ctx, address, node.Key)
	if err != nil {
		return
	}
	changed := false
	permits := false
	cache.mu.Lock()
	if cache.pollers[address] == poller && ctx.Err() == nil {
		now := cache.now()
		before := cache.policies[address].permits(node.Key, now)
		next := nodeState{
			key:       node.Key,
			expires:   time.Unix(int64(descriptor.ExpiresAt), 0),
			anonymous: descriptor.PublicRelay == serviceauthority.RelayAnonymous,
			available: descriptor.Services.Relay,
		}
		permits = next.permits(node.Key, now)
		if permits {
			cache.policies[address] = next
		} else {
			delete(cache.policies, address)
		}
		changed = before != permits
	}
	cache.mu.Unlock()
	// Notify after releasing cache.mu to preserve Service.mu -> nodePolicyCache.mu order.
	// Skip broadcasts when the effective anonymous-admission outcome is unchanged
	// so steady-state descriptor renewals do not spuriously wake observers.
	if changed {
		if cache.diagnostics.Enabled() {
			cache.diagnostics.Emit(diagnostics.New("account_node_policy_changed", diagnostics.Fields{
				"endpoint":          diagnostics.Text(address),
				"permits_anonymous": diagnostics.Bool(permits),
			}).At(cache.now()))
		}
		if cache.onChange != nil {
			cache.onChange()
		}
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
