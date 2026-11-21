package networkprobe

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

func TestSTUNRequiresMappedAddressFromTheRequestedTransaction(t *testing.T) {
	for _, kind := range []string{"binding", "different_transaction", "missing_mapping"} {
		t.Run(kind, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			_ = client.SetDeadline(time.Now().Add(time.Second))
			done := make(chan error, 1)
			go func() {
				var raw [2048]byte
				n, err := server.Read(raw[:])
				if err != nil {
					done <- err
					return
				}
				request := &stun.Message{Raw: raw[:n]}
				if err := request.Decode(); err != nil {
					done <- err
					return
				}
				response := stun.MustBuild(request, stun.BindingSuccess)
				if kind == "different_transaction" {
					response.NewTransactionID()
				}
				if kind != "missing_mapping" {
					err = (&stun.XORMappedAddress{IP: net.IPv4(192, 0, 2, 1), Port: 3478}).AddTo(response)
					if err != nil {
						done <- err
						return
					}
				}
				_, err = server.Write(response.Raw)
				done <- err
			}()
			err := exchangeBinding(client, bindingRTO)
			if setup := <-done; setup != nil {
				t.Fatal(setup)
			}
			if (err == nil) != (kind == "binding") {
				t.Fatalf("binding result %v", err)
			}
		})
	}
}
func TestBindingRetriesLossAndFallsBackToReachableFamily(t *testing.T) {
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	end, _ := ctx.Deadline()
	_ = socket.SetDeadline(end)
	done := make(chan error, 1)
	go func() {
		var packet [2048]byte
		n, _, err := socket.ReadFrom(packet[:])
		if err != nil {
			done <- err
			return
		}
		first := append([]byte(nil), packet[:n]...)
		n, addr, err := socket.ReadFrom(packet[:])
		if err != nil {
			done <- err
			return
		}
		if !bytes.Equal(first, packet[:n]) {
			done <- errors.New("retransmission changed transaction")
			return
		}
		req := &stun.Message{Raw: packet[:n]}
		if err := req.Decode(); err != nil {
			done <- err
			return
		}
		ip := addr.(*net.UDPAddr)
		response, err := stun.Build(req, stun.BindingSuccess, &stun.XORMappedAddress{IP: ip.IP, Port: ip.Port})
		if err == nil {
			_, err = socket.WriteTo(response.Raw, addr)
		}
		done <- err
	}()
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "[::1]:3478" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	winner, err := raceFamilies(ctx, "udp", []string{"[::1]:3478"}, []string{socket.LocalAddr().String()}, bindingDial(dial, 10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer winner.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
