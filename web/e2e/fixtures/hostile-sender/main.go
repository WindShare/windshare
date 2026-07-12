// Command hostile-sender registers an authenticated manifest whose path policy
// violation cannot be produced by the normal CLI. It keeps the E2E oracle at the
// receiver boundary without weakening production sender validation.
package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/relay/protocol"
	transportrelay "github.com/windshare/windshare/transport/relay"
)

const (
	manifestKeyLabel = "windshare/v1 manifest"
	placeholderPath  = "safe-file.txt"
	hostilePath      = "../escape.txt"
	fixtureChunkSize = 64 * 1024
	manifestKeyBytes = 32
)

func main() {
	relayURL := flag.String("relay", "", "relay WebSocket URL")
	frontURL := flag.String("front-url", "", "receiver frontend URL")
	flag.Parse()
	if *relayURL == "" || *frontURL == "" {
		fmt.Fprintln(os.Stderr, "hostile-sender: --relay and --front-url are required")
		os.Exit(2)
	}
	if err := run(*relayURL, *frontURL); err != nil {
		fmt.Fprintf(os.Stderr, "hostile-sender: %v\n", err)
		os.Exit(1)
	}
}

func run(relayURL, frontURL string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	secret := make([]byte, link.ReadSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate read secret: %w", err)
	}
	shareID, err := link.NewShareID(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate share ID: %w", err)
	}
	sealed, err := sealHostileManifest(secret, rand.Reader)
	if err != nil {
		return err
	}
	resumeToken := make([]byte, protocol.ResumeTokenBytes)
	if _, err := rand.Read(resumeToken); err != nil {
		return fmt.Errorf("generate resume token: %w", err)
	}
	connection, err := transportrelay.DialSender(ctx, transportrelay.SenderConfig{
		RelayURL:       relayURL,
		ShareID:        shareID,
		SealedManifest: sealed,
		ResumeToken:    resumeToken,
	})
	if err != nil {
		return fmt.Errorf("register hostile manifest: %w", err)
	}
	defer connection.Close()

	capability := link.Link{
		Suite:      link.SuiteAESGCM,
		ReadSecret: secret,
		ShareID:    shareID,
		Relays:     []string{relayURL},
	}
	url, err := capability.URL(frontURL)
	if err != nil {
		return fmt.Errorf("format capability link: %w", err)
	}
	fmt.Printf("Link: %s\n", url)
	<-ctx.Done()
	return nil
}

func sealHostileManifest(secret []byte, random io.Reader) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, secret, nil, manifestKeyLabel, manifestKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("derive manifest key: %w", err)
	}
	valid := manifest.New(fixtureChunkSize, []manifest.Entry{{
		Path:  placeholderPath,
		Size:  8,
		MTime: 0,
	}})
	sealed, err := manifest.Seal(key, valid, random)
	if err != nil {
		return nil, fmt.Errorf("seal placeholder manifest: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create manifest cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create manifest AEAD: %w", err)
	}
	nonce := sealed[:aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, sealed[aead.NonceSize():], []byte{link.SuiteAESGCM})
	if err != nil {
		return nil, fmt.Errorf("open placeholder manifest: %w", err)
	}
	if bytes.Count(plain, []byte(placeholderPath)) != 1 || len(placeholderPath) != len(hostilePath) {
		return nil, fmt.Errorf("hostile path fixture does not preserve canonical CBOR length")
	}
	plain = bytes.Replace(plain, []byte(placeholderPath), []byte(hostilePath), 1)
	hostileNonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(random, hostileNonce); err != nil {
		return nil, fmt.Errorf("generate hostile manifest nonce: %w", err)
	}
	return aead.Seal(hostileNonce, hostileNonce, plain, []byte{link.SuiteAESGCM}), nil
}
