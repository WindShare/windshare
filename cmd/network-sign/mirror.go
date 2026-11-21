package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/builtinnetwork"
	"github.com/windshare/windshare/internal/networkprobe"
)

const directoryRoute = "/.well-known/windshare-network"

func fetchMirror(authority string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := networkprobe.Client()
	defer client.CloseIdleConnections()
	return networkprobe.Fetch(ctx, client, strings.TrimSuffix(authority, "/")+directoryRoute, nodedirectory.MaximumEnvelopeBytes)
}

func mirror(args []string) error {
	flags := flag.NewFlagSet("mirror", flag.ContinueOnError)
	path := flags.String("out", "", "persistent directory envelope path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("mirror requires --out")
	}
	config, err := builtinnetwork.Config("")
	if err != nil {
		return err
	}
	if config.Authority == nil {
		return errors.New("release has no public authority")
	}
	dir := config.Directory
	embedded, err := nodedirectory.Verify(dir.Embedded, dir.Network, dir.Roots)
	if err != nil {
		return err
	}
	data, err := fetchMirror(config.Authority.URL)
	if err != nil {
		return err
	}
	next, err := nodedirectory.Verify(data, dir.Network, dir.Roots)
	if err != nil {
		return err
	}
	if err = nodedirectory.Accept(embedded.Cursor(), next, time.Now(), 0); err != nil {
		return err
	}
	aliases, err := nodedirectory.RetainAliases(nil, embedded)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(*path)
	if err == nil {
		prior, err := nodedirectory.Verify(current, dir.Network, dir.Roots)
		if err != nil {
			return err
		}
		if err = nodedirectory.Compare(prior.Cursor(), next.Cursor()); err != nil {
			return err
		}
		if prior.Cursor() == next.Cursor() {
			return nil
		}
		aliases, err = nodedirectory.RetainAliases(aliases, prior)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err = nodedirectory.RetainAliases(aliases, next); err != nil {
		return err
	}
	if err = writePublic(*path, next.Envelope()); err != nil {
		return err
	}
	fmt.Printf("event=directory_mirrored generation=%d serial=%d\n", next.Cursor().Generation, next.Cursor().Serial)
	return nil
}
