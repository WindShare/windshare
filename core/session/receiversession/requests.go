package receiversession

import (
	"context"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// requestClient keeps operation ownership in the session core while receiver
// services depend on request completion and cancellation rather than its maps.
type requestClient interface {
	Runtime() *sessioncore.Runtime
	Register() error
	Close()
	HandleMessage(context.Context, protocolsession.Message) error
	Begin(context.Context, protocolsession.MessageKind, []byte) (*sessioncore.Call, error)
	PrepareCall(context.Context, protocolsession.MessageKind, []byte) (*sessioncore.Call, protocolsession.Message, error)
	BeginCall(context.Context, *session.LaneIdentity, *sessioncore.Call, protocolsession.Message) error
	Await(context.Context, *sessioncore.Call) (protocolsession.Message, error)
	AwaitPeer(context.Context, *sessioncore.Call) (protocolsession.Message, error)
	AwaitCall(context.Context, *sessioncore.Call, bool) (protocolsession.Message, error)
	CancelAndEnd(*sessioncore.Call, contentflow.CancelReason) error
	End(*sessioncore.Call)
	SendContinuation(context.Context, *sessioncore.Call, protocolsession.MessageKind, []byte) (protocolsession.SendOutcome, error)
}
