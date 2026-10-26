package sendersession

import (
	"context"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type lifecycle interface {
	Err() error
	Done() <-chan struct{}
	Lifetime() context.Context
	Stopping() bool
	ProtocolSessionID() protocolsession.ProtocolSessionID
	LaneIdentity() (uint32, uint32)
	SetPeerPathControlHandler(func(context.Context, []byte) error)
	SetTURNControlHandler(func(context.Context, []byte) error)
}

// Only lifecycle observations and explicit control-handler installation cross
// the role boundary. The core's lane registry and operation ownership stay private.
func (runtime *Runtime) Err() error {
	if runtime == nil || runtime.lifecycle == nil {
		return sessioncontract.ErrRuntimeClosed
	}
	return runtime.lifecycle.Err()
}
func (runtime *Runtime) Stopping() bool {
	return runtime == nil || runtime.lifecycle == nil || runtime.lifecycle.Stopping()
}
func (runtime *Runtime) Done() <-chan struct{} {
	if runtime == nil || runtime.lifecycle == nil {
		return nil
	}
	return runtime.lifecycle.Done()
}
func (runtime *Runtime) Lifetime() context.Context {
	if runtime == nil || runtime.lifecycle == nil {
		return nil
	}
	return runtime.lifecycle.Lifetime()
}
func (runtime *Runtime) ProtocolSessionID() protocolsession.ProtocolSessionID {
	if runtime == nil || runtime.lifecycle == nil {
		return protocolsession.ProtocolSessionID{}
	}
	return runtime.lifecycle.ProtocolSessionID()
}
func (runtime *Runtime) LaneIdentity() (uint32, uint32) {
	if runtime == nil || runtime.lifecycle == nil {
		return 0, 0
	}
	return runtime.lifecycle.LaneIdentity()
}
func (runtime *Runtime) SetPeerPathControlHandler(handler func(context.Context, []byte) error) {
	if runtime != nil && runtime.lifecycle != nil {
		runtime.lifecycle.SetPeerPathControlHandler(handler)
	}
}
func (runtime *Runtime) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	if runtime != nil && runtime.lifecycle != nil {
		runtime.lifecycle.SetTURNControlHandler(handler)
	}
}
