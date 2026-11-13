package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

type publicKey struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

func keygen(args []string) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	id := flags.String("id", "", "public key identifier")
	private := flags.String("private", "", "new private PEM file, outside the repository")
	public := flags.String("public", "", "public JSON file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *id == "" || *private == "" || *public == "" || *private == *public || flags.NArg() != 0 {
		return errors.New("keygen requires distinct --private, --public and --id")
	}
	if err := distinctPaths(*private, *public); err != nil {
		return err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	defer clear(key)
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	defer clear(encoded)
	file, err := os.OpenFile(*private, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err = pem.Encode(file, &pem.Block{Type: "PRIVATE KEY", Bytes: encoded}); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return writeJSON(*public, publicKey{ID: *id, Key: base64.RawURLEncoding.EncodeToString(pub)})
}

func authorize(args []string) error {
	flags := flag.NewFlagSet("authorize", flag.ContinueOnError)
	rootPath := flags.String("root-private", "", "offline root PEM file")
	rootID := flags.String("root-id", "", "pinned root identifier")
	onlinePath := flags.String("online-public", "", "online public JSON file")
	network := flags.String("network", "windshare", "network identity")
	generation := flags.Uint64("generation", 0, "root-authorized generation")
	notBefore := flags.String("not-before", "", "RFC3339 validity start")
	expires := flags.String("expires", "", "RFC3339 validity end")
	output := flags.String("out", "", "certificate JSON file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := distinctPaths(*rootPath, *onlinePath, *output); err != nil {
		return err
	}
	start, err := time.Parse(time.RFC3339, *notBefore)
	if err != nil {
		return err
	}
	end, err := time.Parse(time.RFC3339, *expires)
	if err != nil || !end.After(start) || end.Sub(start) > nodedirectory.MaximumCertificateLifetime || *generation == 0 || *generation > nodedirectory.MaximumCounter || *network == "" || *output == "" || flags.NArg() != 0 {
		return nodedirectory.ErrFormat
	}
	var online publicKey
	if err := readJSON(*onlinePath, &online); err != nil {
		return err
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(online.Key)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nodedirectory.ErrFormat
	}
	root, err := privateKey(*rootPath)
	if err != nil {
		return err
	}
	defer clear(root)
	certificate, err := nodedirectory.Certify(nodedirectory.Certificate{Format: nodedirectory.Format, Network: *network, Generation: *generation, Key: online.Key, NotBefore: start, ExpiresAt: end}, *rootID, root)
	if err != nil {
		return err
	}
	return writeJSON(*output, certificate)
}
