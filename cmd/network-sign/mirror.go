package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/internal/networkprobe"
)

const directoryRoute = "/.well-known/windshare-network"

func fetchMirror(authority string) ([]byte, error) {
	client := networkprobe.ReleaseClient()
	defer client.CloseIdleConnections()
	return networkprobe.Fetch(context.Background(), client, strings.TrimSuffix(authority, "/")+directoryRoute, nodedirectory.MaximumEnvelopeBytes)
}

func mirror(args []string) error {
	return mirrorWithFetch(args, fetchMirror)
}

func mirrorWithFetch(args []string, fetch func(string) ([]byte, error)) error {
	flags := flag.NewFlagSet("mirror", flag.ContinueOnError)
	path := flags.String("out", "", "persistent directory envelope path")
	bundle := flags.String("bundle", "", "directory in the frontend release being served")
	local := flags.Bool("local-only", false, "reconcile installed snapshots without contacting the authority")
	releasePath := flags.String("release", "", "optional release manifest overriding the embedded network configuration")
	authority := flags.String("authority", "", "optional authority HTTPS origin overriding the release authority URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("mirror requires --out")
	}
	if *local && *authority != "" {
		return errors.New("mirror --authority cannot be combined with --local-only")
	}
	config, err := networkprobe.LoadRelease(*releasePath)
	if err != nil {
		return err
	}
	var remote []byte
	var refreshErr error
	if !*local {
		authorityURL, err := networkprobe.ResolveAuthority(config, *authority)
		if err != nil {
			return err
		}
		remote, refreshErr = fetch(authorityURL)
	}
	// Installation and the timer take the same host lock across selection and rename.
	// Even during an upstream outage a newly installed release can advance the local mirror.
	err = reconcileMirror(config.Directory, *path, *bundle, remote, time.Now())
	if *local && errors.Is(err, nodedirectory.ErrExpired) {
		// An offline frontend release can still serve explicit sharing links.
		fmt.Fprintln(os.Stderr, "event=directory_unavailable reason=expired")
		return nil
	}
	return errors.Join(err, refreshErr)
}

func compareRevision(a, b nodedirectory.Cursor) int {
	if a.Generation != b.Generation {
		return cmp.Compare(a.Generation, b.Generation)
	}
	return cmp.Compare(a.Serial, b.Serial)
}

type installedMirror struct {
	path, retiredPath  string
	published, retired nodedirectory.Cursor
	head               nodedirectory.Snapshot
	aliases            map[string]string
}

func reconcileMirror(dir networkdirectory.Config, out, bundle string, remote []byte, now time.Time) error {
	mirror, err := readInstalledMirror(dir, out, bundle)
	if err != nil {
		return err
	}
	var remoteErr error
	if remote != nil {
		remoteErr = mirror.accept(dir, remote, now)
	}
	return errors.Join(mirror.publish(now), remoteErr)
}

func readInstalledMirror(dir networkdirectory.Config, out, bundle string) (*installedMirror, error) {
	embedded, err := nodedirectory.Verify(dir.Embedded, dir.Network, dir.Roots)
	if err != nil {
		return nil, err
	}
	mirror := &installedMirror{path: out, retiredPath: out + ".expired"}
	candidates := []nodedirectory.Snapshot{embedded}
	for _, path := range []string{out, mirror.retiredPath, bundle} {
		if path == "" {
			continue
		}
		data, err := readBoundedRegular(path, nodedirectory.MaximumEnvelopeBytes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		copy, err := nodedirectory.Verify(data, dir.Network, dir.Roots)
		if err != nil {
			return nil, fmt.Errorf("invalid signed directory %s: %w", path, err)
		}
		candidates = append(candidates, copy)
		switch path {
		case out:
			mirror.published = copy.Cursor()
		case mirror.retiredPath:
			mirror.retired = copy.Cursor()
		}
	}
	// Installing a signed release sets a minimum revision, even after its trust window closes.
	slices.SortFunc(candidates, func(a, b nodedirectory.Snapshot) int { return compareRevision(a.Cursor(), b.Cursor()) })
	for _, copy := range candidates {
		if err := nodedirectory.Compare(mirror.head.Cursor(), copy.Cursor()); err != nil {
			return nil, err
		}
		mirror.aliases, err = nodedirectory.RetainAliases(mirror.aliases, copy)
		if err != nil {
			return nil, err
		}
		mirror.head = copy
	}
	return mirror, nil
}

func (m *installedMirror) accept(dir networkdirectory.Config, remote []byte, now time.Time) error {
	next, err := nodedirectory.Verify(remote, dir.Network, dir.Roots)
	if err != nil {
		return err
	}
	if err := nodedirectory.Accept(m.head.Cursor(), next, now, nodedirectory.DefaultGrace); err != nil {
		return err
	}
	if _, err := nodedirectory.RetainAliases(m.aliases, next); err != nil {
		return err
	}
	m.head = next
	return nil
}

func (m *installedMirror) publish(now time.Time) error {
	if !m.head.Usable(now, nodedirectory.DefaultGrace) {
		return m.withdraw()
	}
	if cursor := m.head.Cursor(); cursor != m.published {
		if err := writePublic(m.path, m.head.Envelope()); err != nil {
			return err
		}
		fmt.Printf("event=directory_mirrored generation=%d serial=%d\n", cursor.Generation, cursor.Serial)
	}
	if m.retired != (nodedirectory.Cursor{}) {
		return removePublic(m.retiredPath)
	}
	return nil
}

func (m *installedMirror) withdraw() error {
	// Persist the signed cursor before withdrawing the public copy. A restart at
	// either point will refuse rollback and finish the interrupted transition.
	if m.retired != m.head.Cursor() {
		if err := writePublic(m.retiredPath, m.head.Envelope()); err != nil {
			return err
		}
	}
	if m.published != (nodedirectory.Cursor{}) {
		if err := removePublic(m.path); err != nil {
			return err
		}
	}
	return nodedirectory.ErrExpired
}
