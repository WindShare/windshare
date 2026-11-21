package networkprobe

import (
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
			err := exchangeBinding(client)
			if setup := <-done; setup != nil {
				t.Fatal(setup)
			}
			if (err == nil) != (kind == "binding") {
				t.Fatalf("binding result %v", err)
			}
		})
	}
}
