// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package turn

import (
	"fmt"
	"net"

	"github.com/pion/turn/v5/internal/client"
	"github.com/pion/turn/v5/internal/proto"
)

// Allocate sends a TURN allocation request to the given transport address.
func (c *Client) Allocate() (net.PacketConn, error) {
	if err := c.allocTryLock.Lock(); err != nil {
		return nil, fmt.Errorf("%w: %s", errOneAllocateOnly, err.Error())
	}
	defer c.allocTryLock.Unlock()

	relayedConn := c.relayedUDPConn()
	if relayedConn != nil {
		return nil, fmt.Errorf("%w: %s", errAlreadyAllocated, relayedConn.LocalAddr().String())
	}

	relayed, lifetime, nonce, reservationToken, err := c.sendAllocateRequest(proto.ProtoUDP)
	if err != nil {
		return nil, err
	}

	relayedAddr := &net.UDPAddr{
		IP:   relayed.IP,
		Port: relayed.Port,
	}

	relayedConn = client.NewUDPConn(&client.AllocationConfig{
		Client:                    c,
		RelayedAddr:               relayedAddr,
		ServerAddr:                c.turnServerAddr,
		Realm:                     c.realm,
		Username:                  c.username,
		Integrity:                 c.integrity,
		Nonce:                     nonce,
		Lifetime:                  lifetime.Duration,
		Net:                       c.net,
		Log:                       c.log,
		PermissionRefreshInterval: c.permissionRefreshInterval,
		BindingRefreshInterval:    c.bindingRefreshInterval,
		OnBindingEvent:            c.onBindingEvent,
		BindingCheckInterval:      c.bindingCheckInterval,
	})
	c.setRelayedUDPConn(relayedConn)
	c.setReservationToken(reservationToken)

	return relayedConn, nil
}

// AllocateTCP creates a new TCP allocation at the TURN server.
func (c *Client) AllocateTCP() (*client.TCPAllocation, error) {
	if err := c.allocTryLock.Lock(); err != nil {
		return nil, fmt.Errorf("%w: %s", errOneAllocateOnly, err.Error())
	}
	defer c.allocTryLock.Unlock()

	allocation := c.getTCPAllocation()
	if allocation != nil {
		return nil, fmt.Errorf("%w: %s", errAlreadyAllocated, allocation.Addr())
	}

	relayed, lifetime, nonce, reservationToken, err := c.sendAllocateRequest(proto.ProtoTCP)
	if err != nil {
		return nil, err
	}

	relayedAddr := &net.TCPAddr{
		IP:   relayed.IP,
		Port: relayed.Port,
	}

	allocation = client.NewTCPAllocation(&client.AllocationConfig{
		Client:      c,
		RelayedAddr: relayedAddr,
		ServerAddr:  c.turnServerAddr,
		Realm:       c.realm,
		Username:    c.username,
		Integrity:   c.integrity,
		Nonce:       nonce,
		Lifetime:    lifetime.Duration,
		Net:         c.net,
		Log:         c.log,
	})

	c.setTCPAllocation(allocation)
	c.setReservationToken(reservationToken)

	return allocation, nil
}

// CreatePermission Issues a CreatePermission request for the supplied addresses
// as described in https://datatracker.ietf.org/doc/html/rfc5766#section-9
func (c *Client) CreatePermission(addrs ...net.Addr) error {
	if conn := c.relayedUDPConn(); conn != nil {
		if err := conn.CreatePermissions(addrs...); err != nil {
			return err
		}
	}

	if allocation := c.getTCPAllocation(); allocation != nil {
		if err := allocation.CreatePermissions(addrs...); err != nil {
			return err
		}
	}

	return nil
}

// OnDeallocated is called when de-allocation of relay address has been complete.
// (Called by UDPConn).
func (c *Client) OnDeallocated(net.Addr) {
	c.setRelayedUDPConn(nil)
	c.setTCPAllocation(nil)
}
