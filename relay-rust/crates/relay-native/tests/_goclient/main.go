// This helper imports the real Go capacity codecs and signs fresh host challenges.
// It is invoked by the Rust gate, so ordinary Go tests do not rebuild a Rust server.
package main

import (
	"flag"
	"fmt"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func main() {
	binary := flag.String("binary", "", "native process to restart")
	database := flag.String("database", "", "durable authority database")
	external := flag.String("endpoint", "", "in-process Rust host endpoint")
	flag.Parse()
	endpoint := *external
	var process *child
	if *binary != "" {
		process = start(*binary, *database, "127.0.0.1:0")
		defer func() { process.kill() }()
		endpoint = process.endpoint
	}
	f := newFixture()
	rejectInvalidProofs(endpoint, f)
	sender := register(endpoint, f, false)
	defer sender.CloseNow()
	duplicate, _ := dial(endpoint, f.init.Registration.ShareID)
	send(duplicate, f.init)
	refusal(duplicate, v2.ErrorAlreadyRegistered)
	_ = duplicate.CloseNow()

	if process != nil {
		address := process.address
		process.kill()
		_ = sender.CloseNow()
		process = start(*binary, *database, address)
		require(endpoint == process.endpoint, "public identity changed during restart")
		sender = register(endpoint, f, true)
		defer sender.CloseNow()
	}
	stop(endpoint, f)
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
	stop(endpoint, f)
	fmt.Println("PASS: live challenges, proofs, descriptor, heartbeat, durable ownership, STOP, restart fencing")
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

	join, _ := dial(endpoint, f.init.Registration.ShareID)
	send(join, v2.DeliveryJoin{ShareID: f.init.Registration.ShareID, ReceiveCapacity: f.init.ReceiveCapacity})
	refusal(join, v2.ErrorUnsupportedMode)
	_ = join.CloseNow()

	wrong := newFixture()
	mismatch, _ := dial(endpoint, wrong.init.Registration.ShareID)
	send(mismatch, f.init)
	// The public routing hint cannot authorize another share's registration.
	refusal(mismatch, v2.ErrorNotFound)
	_ = mismatch.CloseNow()
}
