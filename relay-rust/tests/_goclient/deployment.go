package main

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// The two phases straddle a real deployment so retained STOP is checked through
// the public protocol, independently of Cloudflare's namespace metadata.
type deploymentFixture struct {
	Key        ed25519.PrivateKey
	Init       v2.DeliveryRegisterInit
	Token      v2.ResumeToken
	Descriptor []byte
	StopID     v2.StopID
}

func deploymentProbe(endpoint, phase, path string) {
	require(endpoint != "" && path != "", "deployment probe requires endpoint and state file")
	switch phase {
	case "prepare":
		f := newFixture()
		sender := registerRuntime(endpoint, f, false)
		defer sender.Close()
		stopRuntime(endpoint, f)
		record := deploymentFixture{f.key, f.init, f.token, f.descriptor, f.stopID}
		data, err := json.Marshal(record)
		check(err)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		check(err)
		_, err = file.Write(data)
		check(err)
		check(file.Close())
	case "verify":
		data, err := os.ReadFile(path)
		check(err)
		var record deploymentFixture
		check(json.Unmarshal(data, &record))
		f := fixture{record.Key, record.Init, record.Token, record.Descriptor, record.StopID}
		rejectJoin(endpoint, f, v2.ErrorStopped)
		stopped, _ := dial(endpoint, f.init.Registration.ShareID)
		send(stopped, f.init)
		refusal(stopped, v2.ErrorStopped)
		check(stopped.CloseNow())
		stopRuntime(endpoint, f)
		// The originally imported registration credential must still authorize
		// a new share after code is deployed.
		fresh := newFixture()
		sender := registerRuntime(endpoint, fresh, false)
		defer sender.Close()
		stopRuntime(endpoint, fresh)
	default:
		panic("deployment phase must be prepare or verify")
	}
	fmt.Println("PASS: deployment " + phase)
}
