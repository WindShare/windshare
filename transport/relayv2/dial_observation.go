package relayv2

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

type DialStage struct {
	Stage    string
	Duration time.Duration
	Failure  error
}

func traceDial(ctx context.Context, observe func(DialStage)) context.Context {
	if observe == nil {
		return ctx
	}
	var mu sync.Mutex
	connections := map[string]time.Time{}
	var tlsStarted time.Time
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		ConnectStart: func(network, address string) { mu.Lock(); connections[network+address] = time.Now(); mu.Unlock() },
		ConnectDone: func(network, address string, err error) {
			mu.Lock()
			start := connections[network+address]
			delete(connections, network+address)
			mu.Unlock()
			if !start.IsZero() {
				observe(DialStage{Stage: "tcp", Duration: time.Since(start), Failure: err})
			}
		},
		TLSHandshakeStart: func() { mu.Lock(); tlsStarted = time.Now(); mu.Unlock() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mu.Lock()
			start := tlsStarted
			mu.Unlock()
			if !start.IsZero() {
				observe(DialStage{Stage: "tls", Duration: time.Since(start), Failure: err})
			}
		},
	})
}
