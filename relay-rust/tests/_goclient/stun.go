package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pion/stun/v4"
)

// Reuse the lifecycle helper's existing build to check an independent codec on
// real IPv4/IPv6 sockets every time the native binary is started or restarted.
func verifySTUN(addresses []string, admin string) {
	require(len(addresses) == 2, "native STUN listeners missing from readiness")
	for _, address := range addresses {
		verifyBinding(address)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/healthz", "/metrics"} {
		response, err := client.Get("http://" + admin + path)
		check(err)
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		check(err)
		require(response.StatusCode == http.StatusOK, "native STUN admin unavailable")
		if path == "/metrics" {
			require(strings.Contains(string(data), "windshare_stun_responded_total"), "STUN counters missing")
		}
	}
}

func verifyBinding(address string) {
	socket, err := net.Dial("udp", address)
	check(err)
	defer socket.Close()
	check(socket.SetDeadline(time.Now().Add(3 * time.Second)))
	// Fingerprint is optional in requests; exercise both forms.
	for _, fingerprint := range []bool{false, true} {
		request := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
		if fingerprint {
			check(stun.Fingerprint.AddTo(request))
		}
		_, err = socket.Write(request.Raw)
		check(err)
		buffer := make([]byte, 1200)
		length, err := socket.Read(buffer)
		check(err)
		response := &stun.Message{Raw: buffer[:length]}
		check(response.Decode())
		require(response.Type == stun.BindingSuccess && response.TransactionID == request.TransactionID,
			"native Binding response changed transaction")
		check(stun.Fingerprint.Check(response))
		var mapped stun.XORMappedAddress
		check(mapped.GetFrom(response))
		local := socket.LocalAddr().(*net.UDPAddr)
		require(mapped.IP.Equal(local.IP) && mapped.Port == local.Port, "native Binding address mismatch")
	}
}
