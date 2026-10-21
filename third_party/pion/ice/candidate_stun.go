// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package ice

import (
	"errors"
	"io"
	"time"
)

const maxQueuedSTUNPackets = 32

var (
	errSTUNQueueFull      = errors.New("candidate STUN queue full")
	errSTUNPacketTooLarge = errors.New("candidate STUN packet exceeds receive MTU")
)

type candidateSTUNPacket struct {
	raw     []byte
	remote  Candidate
	expires time.Time
}

type candidateSTUNWriter struct {
	queue chan candidateSTUNPacket
	done  chan struct{}
}

func (c *candidateBase) startSTUNWriter() {
	if c.Type() != CandidateTypeRelay {
		return
	}
	c.stunWriter = &candidateSTUNWriter{
		queue: make(chan candidateSTUNPacket, maxQueuedSTUNPackets),
		done:  make(chan struct{}),
	}
	go c.runSTUNWriter()
}

func (c *candidateBase) writeSTUN(raw []byte, remote Candidate) error {
	if c.stunWriter == nil {
		_, err := c.writeTo(raw, remote)
		return err
	}
	select {
	case <-c.closeCh:
		return io.ErrClosedPipe
	default:
	}
	if len(raw) > receiveMTU {
		return errSTUNPacketTooLarge
	}
	// TURN may synchronously create a peer permission before writing. That
	// round trip must not stall ICE's event loop or another local candidate.
	packet := candidateSTUNPacket{
		raw: append([]byte(nil), raw...), remote: remote,
		expires: time.Now().Add(maxBindingRequestTimeout),
	}
	select {
	case <-c.closeCh:
		return io.ErrClosedPipe
	case c.stunWriter.queue <- packet:
		return nil
	default:
		// ICE retransmits checks; bounded loss is preferable to blocking the
		// same loop that must process responses and choose a working path.
		return errSTUNQueueFull
	}
}

func (c *candidateBase) runSTUNWriter() {
	defer close(c.stunWriter.done)
	for {
		select {
		case <-c.closeCh:
			return
		default:
		}
		select {
		case <-c.closeCh:
			return
		case packet := <-c.stunWriter.queue:
			// A permission timeout can outlive pending ICE transactions. Do not
			// flood the server with obsolete checks when that write unblocks.
			if !time.Now().Before(packet.expires) {
				c.agent().log.Debugf("Discard expired STUN write from %s to %s", c, packet.remote)
				continue
			}
			if _, err := c.writeTo(packet.raw, packet.remote); err != nil {
				c.agent().log.Tracef("Failed to send queued STUN message: %s", err)
			}
		}
	}
}
