// Package turnsession owns the lifetime of TURN authorization for one authenticated share session.
package turnsession

import (
	"context"
	"sync"
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

type Credential struct {
	ServiceID          string
	URLs               []string
	Username, Password string
	ValidFor           time.Duration
}

// Reusable contains only live grants already owned by the requesting session.
type IssueRequest struct {
	Trace      Trace
	Mode       p.TURNMode
	ServiceIDs []string
	Reusable   map[string]bool
}
type IssueResult struct {
	Retained    []string
	Status      p.TURNStatus
	HasFallback bool
	Credentials []Credential
}

type Provider interface {
	Issue(context.Context, IssueRequest) IssueResult
}

// Capacity covers negotiation through authenticated attachment. An established
// lane owns its transport independently and no longer consumes this permit.
type Capacity struct{ permits chan struct{} }

func NewCapacity() *Capacity { return &Capacity{permits: make(chan struct{}, p.MaxGrantsPerMessage)} }
func (c *Capacity) Acquire(ctx context.Context) (func(), error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	select {
	case c.permits <- struct{}{}:
		return sync.OnceFunc(func() { <-c.permits }), nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}
