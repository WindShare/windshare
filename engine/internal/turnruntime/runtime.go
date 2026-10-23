package turnruntime

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/windshare/windshare/engine/relayprofile"
)

// Runtime owns account ledgers and discovery for one Engine lifetime. Settings
// replacement never replaces this owner or resets its accumulated usage.
type Runtime struct {
	mu           sync.Mutex
	accounts     *accountManager
	discovery    *discovery
	dependencies Dependencies
	settings     Settings
	closed       bool
	closeOnce    sync.Once
	closeErr     error
}

func New(settings Settings, dependencies Dependencies) (*Runtime, error) {
	prepared, err := prepareSettings(settings)
	if err != nil {
		return nil, err
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	accounts, err := newAccountManager(accountConfig{Now: dependencies.Now, Observe: dependencies.Observe})
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{accounts: accounts, dependencies: dependencies}
	runtime.discovery = newDiscovery(accounts, dependencies)
	if err := runtime.apply(prepared); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	return runtime, nil
}

// Configure validates both manual and managed settings before publishing either.
// It does not contact issuers; existing leases retain their issuing source.
func (runtime *Runtime) Configure(settings Settings) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed {
		return ErrClosed
	}
	prepared, err := prepareSettings(settings)
	if err != nil {
		return err
	}
	return runtime.apply(prepared)
}

func (runtime *Runtime) apply(prepared preparedSettings) error {
	// Relay access keys are intentionally omitted from JSON; equality must include them.
	if reflect.DeepEqual(runtime.settings, prepared.settings) {
		return nil
	}
	manual, err := prepared.manual.bind(runtime.dependencies, runtime.accounts.readConfiguration())
	if err != nil {
		return err
	}
	if err := runtime.discovery.configure(manual, prepared.settings.Managed); err != nil {
		return err
	}
	runtime.settings = prepared.settings
	return nil
}

// ForShare binds the selected deployments and registers discovery demand without
// waiting for remote discovery. Releasing one share cannot stop another's worker.
func (runtime *Runtime) ForShare(ctx context.Context, relays []relayprofile.Candidate) (*Selection, func()) {
	addresses := make([]string, 0, len(relays))
	for _, relay := range relays {
		addresses = append(addresses, relay.PublicURL)
	}
	selection := runtime.accounts.Select(addresses)
	return selection, runtime.discovery.Watch(ctx, relays)
}

func (runtime *Runtime) Status() []Status { return runtime.accounts.Snapshot() }

func (runtime *Runtime) QueryUsage(ctx context.Context, account string, first, last time.Time) (Usage, error) {
	return runtime.accounts.QueryUsage(ctx, account, first, last)
}

func (runtime *Runtime) Close() error {
	runtime.closeOnce.Do(func() {
		runtime.mu.Lock()
		runtime.closed = true
		runtime.mu.Unlock()
		runtime.discovery.Close()
		runtime.closeErr = runtime.accounts.Close()
	})
	return runtime.closeErr
}
