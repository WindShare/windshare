// This helper drives the production Go relayv2 API against the native Rust host.
// It is invoked by the Rust gate, so ordinary Go tests do not rebuild a Rust server.
package main

import (
	"flag"
	"fmt"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func main() {
	binary := flag.String("binary", "", "native process to restart")
	database := flag.String("database", "", "durable authority database")
	external := flag.String("endpoint", "", "Rust host endpoint")
	preview := flag.Bool("preview", false, "Cloudflare Preview delivery and recovery scenario")
	idle := flag.Duration("hibernate-idle", 20*time.Second, "bounded hibernation observation interval")
	flag.Parse()
	if *preview {
		cloudflare(*external, *idle)
		return
	}
	endpoint := *external
	var process *child
	if *binary != "" {
		process = start(*binary, *database, "127.0.0.1:0")
		defer func() { process.kill() }()
		endpoint = process.endpoint
	}
	f := newDeliveryFixture()
	// Two simultaneous sessions need room for both native per-flow windows;
	// the receiver still declares an asymmetric two-envelope byte budget.
	f.init.ReceiveCapacity = v2.ReceiveCapacity{Frames: 32, EncodedBytes: 32 * maximumEnvelopeBytes}
	rejectInvalidProofs(endpoint, f)
	rejectJoin(endpoint, f, v2.ErrorNotFound)
	sender := registerRuntime(endpoint, f, false)
	defer sender.Close()
	duplicate, _ := dial(endpoint, f.init.Registration.ShareID)
	send(duplicate, f.init)
	refusal(duplicate, v2.ErrorAlreadyRegistered)
	_ = duplicate.CloseNow()
	delivery := transferRuntime(endpoint, f, sender)
	defer func() { delivery.close() }()

	if process != nil {
		address := process.address
		process.kill()
		delivery.close()
		_ = sender.Close()
		process = start(*binary, *database, address)
		require(endpoint == process.endpoint, "public identity changed during restart")
		sender = registerRuntime(endpoint, f, true)
		defer sender.Close()
		delivery = transferRuntime(endpoint, f, sender)
	}
	stopRuntime(endpoint, f)
	delivery.stopped(sender)
	if process != nil {
		address := process.address
		process.kill()
		process = start(*binary, *database, address)
	}
	for _, mode := range []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationResume} {
		stopped, _ := dial(endpoint, f.init.Registration.ShareID)
		init := f.init
		init.Registration.Mode = mode
		send(stopped, init)
		// RESUME validates its token before it asks the lifecycle for admission.
		if mode == v2.RegistrationResume {
			send(stopped, v2.ResumeCredential{Token: f.token})
		}
		refusal(stopped, v2.ErrorStopped)
		_ = stopped.CloseNow()
	}
	stopRuntime(endpoint, f)
	fmt.Println("PASS: real relayv2 clients, bidirectional delivery beyond asymmetric windows, sparse feedback, slow-receiver isolation, queue settlement, STOP and restart/resume")
}

func rejectInvalidProofs(endpoint string, f fixture) {
	invalid, route := dial(endpoint, f.init.Registration.ShareID)
	send(invalid, f.init)
	first := challenge(invalid)
	signature := proof(f, f.init, first, route.Identity)
	signature.Signature[0] ^= 1
	send(invalid, signature)
	closed(invalid)
	_ = invalid.CloseNow()

	corrupt, route := dial(endpoint, f.init.Registration.ShareID)
	send(corrupt, f.init)
	second := challenge(corrupt)
	require(first.ID != second.ID && first.Nonce != second.Nonce, "challenge entropy was reused")
	send(corrupt, proof(f, f.init, second, route.Identity))
	send(corrupt, v2.DescriptorUpload{Object: []byte("different descriptor")})
	closed(corrupt)
	_ = corrupt.CloseNow()

	wrong := newFixture()
	mismatch, _ := dial(endpoint, wrong.init.Registration.ShareID)
	send(mismatch, f.init)
	// The public routing hint cannot authorize another share's registration.
	refusal(mismatch, v2.ErrorNotFound)
	_ = mismatch.CloseNow()
}

func rejectJoin(endpoint string, f fixture, code v2.ErrorCode) {
	join, _ := dial(endpoint, f.init.Registration.ShareID)
	defer join.CloseNow()
	send(join, v2.DeliveryJoin{ShareID: f.init.Registration.ShareID, ReceiveCapacity: f.init.ReceiveCapacity})
	refusal(join, code)
}
