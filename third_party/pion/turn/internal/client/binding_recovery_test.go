// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package client

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/logging"
)

func recoveryFixture() (*UDPConn, *binding) {
	conn := &UDPConn{closeCh: make(chan struct{}), bindingRefreshInterval: defaultBindingRefreshInterval,
		allocation: allocation{relayedAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1000},
			log: logging.NewDefaultLoggerFactory().NewLogger("test")}}
	bound := newBindingManager().create(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2000})
	bound.lastUsed.Store(time.Now().UnixNano())
	return conn, bound
}

func TestBindingFailureScopesAndRetryBudget(t *testing.T) {
	for _, code := range []int{400, 403, 437, 500, 508} {
		t.Run(channelBindResponseError{code}.Error(), func(t *testing.T) {
			conn, bound := recoveryFixture()
			bound.setRefreshedAt(time.Now())
			bound.setState(bindingStateReady)
			var event BindingEvent
			conn.onBindingEvent = func(value BindingEvent) { event = value }
			conn.handleBindChannelError(bound, bindingStateReady, channelBindResponseError{code})
			switch code {
			case 400, 437:
				if bound.ok() || event.Status != BindingMismatch {
					t.Fatal("untrusted mapping remained usable")
				}
				if _, started := conn.startBinding(bound); started {
					t.Fatal("mismatch retried in the same allocation")
				}
			case 403:
				if bound.ok() || event.Status != BindingRejected {
					t.Fatal("permanent rejection was not isolated")
				}
			case 500, 508:
				if !bound.ok() || event.Status != BindingRetrying {
					t.Fatal("transient response destroyed confirmed mapping")
				}
				if _, started := conn.startBinding(bound); started {
					t.Fatal("retry ignored backoff")
				}
				for range maximumBindingFailures - 1 {
					conn.handleBindChannelError(bound, bindingStateReady, channelBindResponseError{code})
				}
				if event.Status != BindingExhausted || bound.ok() {
					t.Fatal("retry budget was not finite")
				}
			}
		})
	}
}

func TestUnknownBindingRetainsNumberWithoutInventingSuccess(t *testing.T) {
	conn, bound := recoveryFixture()
	number := bound.number
	conn.handleBindChannelError(bound, bindingStateIdle, errors.Join(errChannelBindTransactionFailed, errors.New("lost response")))
	if bound.ok() {
		t.Fatal("a new binding was treated as confirmed after timeout")
	}
	bound.retryAt = time.Now().Add(-time.Second)
	if _, started := conn.startBinding(bound); !started || bound.number != number {
		t.Fatal("retry changed peer mapping")
	}
}

func TestExpiredBindingStopsChannelDataAndIdleBindingsDoNotRefresh(t *testing.T) {
	conn, bound := recoveryFixture()
	bound.setState(bindingStateReady)
	bound.setRefreshedAt(time.Now().Add(-channelBindingLifetime))
	if bound.ok() {
		t.Fatal("expired binding can still send ChannelData")
	}
	bound.lastUsed.Store(time.Now().Add(-bindingDemandLifetime).UnixNano())
	if _, started := conn.startBinding(bound); started {
		t.Fatal("unused probe binding refreshed")
	}
}

func TestConcurrentFirstWritesShareOneChannel(t *testing.T) {
	mgr := newBindingManager()
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2000}
	var work sync.WaitGroup
	bindings := make(chan *binding, 64)
	for range cap(bindings) {
		work.Add(1)
		go func() { defer work.Done(); bindings <- mgr.create(addr) }()
	}
	work.Wait()
	close(bindings)
	first := <-bindings
	for bound := range bindings {
		if bound != first {
			t.Fatal("one peer acquired competing channels")
		}
	}
	if mgr.size() != 1 {
		t.Fatal("duplicate mapping retained")
	}
}
