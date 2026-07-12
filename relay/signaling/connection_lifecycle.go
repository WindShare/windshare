package signaling

import (
	"context"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/protocol"
)

// shutdown starts an idempotent asynchronous close sequence because callers
// may hold the Hub lock or run inside the peer connection's read loop.
func (c *conn) shutdown(flushFirst bool) {
	c.closeOnce.Do(func() {
		go func() {
			defer close(c.closedCh)
			if flushFirst {
				ctx, cancel := context.WithTimeout(context.Background(), finalFlushTimeout)
				c.pump.WaitIdle(ctx)
				cancel()
			}
			c.pump.Close()
			c.cancel()
			if flushFirst {
				_ = c.ws.Close(websocket.StatusNormalClosure, "")
			} else {
				// Hub shutdown promises a forced disconnect. Waiting for a peer that
				// does not participate in the close handshake would retain the HTTP
				// handler and its admission lease for the library's five-second bound.
				_ = c.ws.CloseNow()
			}
		}()
	})
}

func (c *conn) closeAfterFlush() { c.shutdown(true) }

func (c *conn) closeNow() { c.shutdown(false) }

func (c *conn) fatal(code, reason string) {
	c.sendConnection(protocol.NewError(code, reason))
	c.closeAfterFlush()
}

// ServeConn owns an admitted, upgraded connection until its close sequence is
// complete. The HTTP handler acquires lease before upgrade so capacity also
// bounds handshake work.
func (h *Hub) ServeConn(ctx context.Context, ws *websocket.Conn, shareID, remoteIP string, lease *ConnectionLease) {
	if lease == nil || lease.hub != h {
		_ = ws.CloseNow()
		return
	}
	defer lease.Release()
	cctx, cancel := context.WithCancel(ctx)
	c := &conn{
		ws:                ws,
		ctx:               cctx,
		cancel:            cancel,
		ip:                remoteIP,
		inactivityTimeout: h.cfg.KeepaliveTimeout,
		closedCh:          make(chan struct{}),
	}
	c.pump = forward.NewPump(wsWriter{c}, h.cfg.Pump)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.closeNow()
		<-c.closedCh
		return
	}
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
		c.closeNow()
		<-c.closedCh
	}()

	if err := protocol.ValidateShareID(shareID); err != nil {
		c.fatal(protocol.ErrCodeProtocol, "invalid shareId in request path")
		return
	}

	ws.SetReadLimit(protocol.MaxSignalingMessageBytes)
	hctx, hcancel := context.WithTimeout(cctx, h.cfg.RoleTimeout)
	typ, data, err := ws.Read(hctx)
	hcancel()
	if err != nil {
		return
	}
	if typ != websocket.MessageText {
		c.fatal(protocol.ErrCodeProtocol, "first message must be register/join JSON")
		return
	}
	msg, err := protocol.Decode(data)
	if err != nil {
		c.fatal(protocol.ErrCodeProtocol, err.Error())
		return
	}
	switch m := msg.(type) {
	case *protocol.Register:
		h.serveSender(c, shareID, m)
	case *protocol.Join:
		h.serveReceiver(c, shareID, m)
	default:
		c.fatal(protocol.ErrCodeProtocol, "first message must be register or join")
	}
}
