package signaling

import (
	"bytes"
	"context"
	"io"
	"math"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/relay/protocol"
)

const (
	// Slack lets a slightly oversized upload receive a semantic protocol error;
	// the WebSocket read limit still terminates grossly oversized messages.
	manifestReadSlack = 64 * 1024
	// Scratch memory stays fixed while every retained payload chunk is charged
	// to admission before it is appended.
	manifestReadChunkSize = 32 * 1024
)

// dataReadLimit bounds ordinary signaling/data after role establishment.
func (h *Hub) dataReadLimit() int64 {
	limit := h.cfg.MaxFrameSize
	if limit > math.MaxInt64-protocol.ForwardOverheadBytes {
		limit = math.MaxInt64
	} else {
		limit += protocol.ForwardOverheadBytes
	}
	return max(limit, protocol.MaxSignalingMessageBytes)
}

func (h *Hub) manifestReadLimit() int64 {
	overhead := int64(protocol.ManifestOverheadBytes + manifestReadSlack)
	if h.cfg.MaxManifestSize > math.MaxInt64-overhead {
		return math.MaxInt64
	}
	return h.cfg.MaxManifestSize + overhead
}

// readRegisterManifest streams the upload instead of using Conn.Read, whose
// all-at-once allocation would happen before admission could account for it.
// New manifests reserve every retained chunk first. Resume uploads compare
// directly with the immutable retained bytes and never allocate a duplicate.
func (h *Hub) readRegisterManifest(c *conn, attempt *registerAttempt) ([]byte, bool) {
	c.ws.SetReadLimit(h.manifestReadLimit())
	hctx, hcancel := context.WithTimeout(c.ctx, h.cfg.RoleTimeout)
	defer hcancel()
	typ, reader, err := c.ws.Reader(hctx)
	if err != nil {
		return nil, false
	}
	if typ != websocket.MessageBinary {
		c.fatal(protocol.ErrCodeProtocol, "register must be followed by a manifest binary frame")
		return nil, false
	}
	var kind [protocol.ManifestOverheadBytes]byte
	if _, err := io.ReadFull(reader, kind[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			c.fatal(protocol.ErrCodeProtocol, "invalid manifest frame")
		}
		return nil, false
	}
	if kind[0] != protocol.BinTypeManifest {
		c.fatal(protocol.ErrCodeProtocol, "register must be followed by a manifest binary frame")
		return nil, false
	}

	buf := make([]byte, manifestReadChunkSize)
	if attempt != nil && attempt.existing != nil {
		return h.compareRetainedManifest(c, reader, buf, attempt.existing.manifestFrame)
	}
	return h.readNewManifest(c, reader, buf, attempt)
}

func (h *Hub) compareRetainedManifest(c *conn, reader io.Reader, buf, manifestFrame []byte) ([]byte, bool) {
	expected := manifestPayload(manifestFrame)
	offset := 0
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if int64(n) > h.cfg.MaxManifestSize-int64(offset) ||
				n > len(expected)-min(offset, len(expected)) ||
				!bytes.Equal(buf[:n], expected[offset:offset+n]) {
				c.fatal(protocol.ErrCodeResumeRejected, "reconnect manifest does not match retained share")
				return nil, false
			}
			offset += n
		}
		if readErr == io.EOF {
			if offset != len(expected) {
				c.fatal(protocol.ErrCodeResumeRejected, "reconnect manifest does not match retained share")
				return nil, false
			}
			return manifestFrame, true
		}
		if readErr != nil {
			return nil, false
		}
	}
}

func (h *Hub) readNewManifest(c *conn, reader io.Reader, buf []byte, attempt *registerAttempt) ([]byte, bool) {
	manifestFrame := []byte{protocol.BinTypeManifest}
	var total int64
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if int64(n) > h.cfg.MaxManifestSize-total {
				c.fatal(protocol.ErrCodeManifestTooLarge, "manifest exceeds relay limit")
				return nil, false
			}
			if attempt != nil && attempt.reservation != nil {
				if decision := attempt.reservation.ReserveManifestBytes(int64(n)); !decision.Allowed() {
					c.fatal(protocol.ErrCodeManifestBudget, "relay manifest capacity exhausted; retry later or use another relay")
					return nil, false
				}
			}
			manifestFrame = append(manifestFrame, buf[:n]...)
			total += int64(n)
		}
		if readErr == io.EOF {
			return manifestFrame, true
		}
		if readErr != nil {
			return nil, false
		}
	}
}
