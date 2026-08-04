// Package testloopback owns deterministic loopback resources used by tests.
// Production packages must receive the resulting interfaces through their
// normal dependency-injection seams rather than importing this package.
package testloopback

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

// ErrClosed makes attempts to create resources after cleanup distinguishable
// from OS bind failures.
var ErrClosed = errors.New("loopback fixture is closed")

type testContext interface {
	Helper()
	Cleanup(func())
	Errorf(string, ...any)
	Fatalf(string, ...any)
}

type ownedResource struct {
	name   string
	closer io.Closer
}

// Fixture is the single cleanup owner for every resource created through it.
// Reverse-order retirement keeps higher-level Pion owners ahead of the sockets
// they depend on while still making every close failure part of the test verdict.
type Fixture struct {
	t testContext

	mu        sync.Mutex
	resources []ownedResource
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

func New(t testContext) *Fixture {
	t.Helper()
	fixture := &Fixture{t: t}
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("close loopback fixture: %v", err)
		}
	})
	return fixture
}

func (fixture *Fixture) Close() error {
	if fixture == nil {
		return nil
	}
	fixture.closeOnce.Do(func() {
		fixture.mu.Lock()
		fixture.closed = true
		resources := append([]ownedResource(nil), fixture.resources...)
		fixture.resources = nil
		fixture.mu.Unlock()

		var failures []error
		for index := range slices.Backward(resources) {
			resource := resources[index]
			if err := resource.closer.Close(); err != nil {
				failures = append(failures, fmt.Errorf("close %s: %w", resource.name, err))
			}
		}
		fixture.closeErr = errors.Join(failures...)
	})
	return fixture.closeErr
}

func (fixture *Fixture) own(name string, closer io.Closer) error {
	if fixture == nil || closer == nil || name == "" {
		return errors.New("loopback resource ownership is invalid")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.closed {
		return ErrClosed
	}
	fixture.resources = append(fixture.resources, ownedResource{name: name, closer: closer})
	return nil
}
