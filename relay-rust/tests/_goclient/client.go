package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const operationTimeout = 5 * time.Second

type fixture struct {
	key        ed25519.PrivateKey
	init       v2.DeliveryRegisterInit
	token      v2.ResumeToken
	descriptor []byte
	stopID     v2.StopID
}

func newFixture() fixture {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	check(err)
	keyHash := sha256.Sum256(append([]byte("windshare/v2 sender-key\x00"), public...))
	var pkHash v2.PKHash
	copy(pkHash[:], keyHash[:])
	shareHash := sha256.Sum256(append([]byte("windshare/v2 share-id\x00"), pkHash[:]...))
	var share v2.ShareID
	copy(share[:], shareHash[:])
	f := fixture{key: private, descriptor: []byte("opaque sealed descriptor: the relay authenticates only its digest")}
	fill(f.token[:])
	fill(f.stopID[:])
	var instance v2.ShareInstance
	fill(instance[:])
	f.init = v2.DeliveryRegisterInit{
		Registration: v2.RegisterInit{
			Mode: v2.RegistrationFresh, ShareID: share, ShareInstance: instance,
			PKHash: pkHash, DescriptorDigest: sha256.Sum256(f.descriptor),
			ResumeTokenHash: sha256.Sum256(f.token[:]),
		},
		ReceiveCapacity: v2.ReceiveCapacity{Frames: 16, EncodedBytes: 16 * (v2.MaxOpaqueCiphertextBytes + v2.OpaqueRouteHeaderBytes)},
	}
	return f
}

func fill(bytes []byte) { _, err := rand.Read(bytes); check(err) }
func check(err error) {
	if err != nil {
		panic(err)
	}
}
func require(ok bool, message string) {
	if !ok {
		panic(message)
	}
}

func dial(endpoint string, share v2.ShareID) (*websocket.Conn, v2.RelayEndpoint) {
	return dialWithCredential(endpoint, share, true)
}

func dialWithCredential(endpoint string, share v2.ShareID, credential bool) (*websocket.Conn, v2.RelayEndpoint) {
	route, err := v2.RelayEndpointForShare(strings.TrimSuffix(endpoint, v2.WebSocketPath), share)
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	headers := http.Header{}
	if secret := os.Getenv("WINDSHARE_RELAY_REGISTRATION_SECRET"); credential && secret != "" {
		headers.Set("Authorization", "Bearer "+secret)
	}
	connection, _, err := websocket.Dial(ctx, route.DialURL, &websocket.DialOptions{HTTPHeader: headers})
	check(err)
	connection.SetReadLimit(v2.MaxDescriptorBytes + v2.DescriptorUploadHeaderBytes)
	return connection, route
}

func send(connection *websocket.Conn, frame encoding.BinaryMarshaler) {
	encoded, err := frame.MarshalBinary()
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	check(connection.Write(ctx, websocket.MessageBinary, encoded))
}

func read(connection *websocket.Conn) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	kind, bytes, err := connection.Read(ctx)
	check(err)
	require(kind == websocket.MessageBinary, "nonbinary server response")
	return bytes
}

func challenge(connection *websocket.Conn) v2.Challenge {
	challenge, err := v2.ParseChallenge(read(connection))
	check(err)
	expiry := time.Unix(int64(challenge.ExpiresAtUnixSeconds), 0)
	require(expiry.After(time.Now()) && expiry.Before(time.Now().Add(15*time.Second)), "challenge does not use live clock")
	return challenge
}

func proof(f fixture, init v2.DeliveryRegisterInit, challenge v2.Challenge, identity v2.RelayIdentity) v2.RegisterProof {
	preimage, err := v2.DeliveryRegistrationPreimage(init, challenge, identity)
	check(err)
	result := v2.RegisterProof{Mode: init.Registration.Mode}
	copy(result.SenderPublicKey[:], f.key.Public().(ed25519.PublicKey))
	copy(result.Signature[:], ed25519.Sign(f.key, preimage))
	return result
}

func register(endpoint string, f fixture, resume bool) *websocket.Conn {
	connection, route := dialWithCredential(endpoint, f.init.Registration.ShareID, !resume)
	init := f.init
	if resume {
		init.Registration.Mode = v2.RegistrationResume
	}
	send(connection, init)
	if resume {
		send(connection, v2.ResumeCredential{Token: f.token})
	}
	send(connection, proof(f, init, challenge(connection), route.Identity))
	if !resume {
		send(connection, v2.DescriptorUpload{Object: f.descriptor})
	}
	ack, err := v2.ParseRegistered(read(connection))
	check(err)
	require(ack.ShareID == init.Registration.ShareID &&
		ack.ShareInstance == init.Registration.ShareInstance &&
		ack.DescriptorDigest == init.Registration.DescriptorDigest, "wrong REGISTERED authority")
	send(connection, v2.ConnectionProbe{Nonce: 1})
	heartbeat, err := v2.ParseConnectionProbeAck(read(connection))
	check(err)
	require(heartbeat.Nonce == 1, "heartbeat nonce mismatch")
	return connection
}

func stop(endpoint string, f fixture) {
	connection, route := dialWithCredential(endpoint, f.init.Registration.ShareID, false)
	defer connection.CloseNow()
	registration := f.init.Registration
	init := v2.StopInit{ShareID: registration.ShareID, ShareInstance: registration.ShareInstance,
		PKHash: registration.PKHash, RelayIdentity: route.Identity, StopID: f.stopID}
	send(connection, init)
	signed, err := v2.NewStopProof(init, challenge(connection), f.key)
	check(err)
	send(connection, signed)
	stopped, err := v2.ParseStopped(read(connection))
	check(err)
	require(stopped.StopID == f.stopID, "wrong STOPPED transaction")
}

func closed(connection *websocket.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	_, _, err := connection.Read(ctx)
	require(err != nil && ctx.Err() == nil, "invalid authentication was not closed promptly")
}

func refusal(connection *websocket.Conn, code v2.ErrorCode) {
	result, err := v2.ParseError(read(connection))
	check(err)
	require(result.Code == code, fmt.Sprintf("refusal %d, expected %d", result.Code, code))
}
