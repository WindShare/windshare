package receiversession

import (
	"context"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type testLifecycle struct {
	lifecycle
	sessionID protocolsession.ProtocolSessionID
	ctx       context.Context
	done      <-chan struct{}
	err       error
}

func (life testLifecycle) Lifetime() context.Context { return life.ctx }
func (life testLifecycle) Done() <-chan struct{}     { return life.done }
func (life testLifecycle) Err() error                { return life.err }
func (life testLifecycle) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return life.sessionID
}
