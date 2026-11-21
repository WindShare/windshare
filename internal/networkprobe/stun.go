package networkprobe

import (
	"context"
	"errors"
	"net"
	"os"
	"time"

	"github.com/pion/stun/v4"
)

const (
	bindingRTO      = 500 * time.Millisecond
	bindingAttempts = 3
)

func binding(ctx context.Context, address string) error {
	conn, err := dialPublic(ctx, "udp", address, bindingDial((&net.Dialer{}).DialContext, bindingRTO))
	if err != nil {
		return err
	}
	return conn.Close()
}

// A UDP socket is usable only after a matching STUN reply, so the address race
// includes the exchange itself. Each retransmission reuses its transaction ID.
func bindingDial(dial dialFunc, initialRTO time.Duration) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		err = exchangeBinding(conn, initialRTO)
		stop()
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func exchangeBinding(conn net.Conn, initialRTO time.Duration) error {
	request, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return err
	}
	var packet [2048]byte
	for attempt := range bindingAttempts {
		if err := conn.SetDeadline(time.Now().Add(initialRTO << attempt)); err != nil {
			return err
		}
		if _, err := conn.Write(request.Raw); err != nil {
			return err
		}
		n, err := conn.Read(packet[:])
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			return err
		}
		response := stun.Message{Raw: packet[:n]}
		if err := response.Decode(); err != nil {
			return err
		}
		var mapped stun.XORMappedAddress
		if response.Type != stun.BindingSuccess || response.TransactionID != request.TransactionID || mapped.GetFrom(&response) != nil {
			return errors.New("invalid binding reply")
		}
		return nil
	}
	return errors.New("STUN binding timed out")
}
