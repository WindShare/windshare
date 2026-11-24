package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/builtinnetwork"
	"github.com/windshare/windshare/engine/networkconfig"
)

// Allow a maximum-size signed directory envelope plus surrounding manifest
// metadata while still capping accidental or untrusted large file reads.
const maxReleaseManifestBytes = nodedirectory.MaximumEnvelopeBytes + 64<<10

func parseHTTPSOrigin(raw string) (*url.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsRune(raw, '#') || strings.ContainsRune(raw, '\\') {
		return nil, errors.New("invalid HTTPS origin")
	}
	base, err := url.Parse(raw)
	if err != nil || base.Scheme != "https" || base.Opaque != "" || base.Hostname() == "" || strings.HasSuffix(base.Host, ":") ||
		base.User != nil || base.ForceQuery || base.RawQuery != "" || base.Fragment != "" ||
		(base.EscapedPath() != "" && base.EscapedPath() != "/") {
		return nil, errors.New("invalid HTTPS origin")
	}
	if port := base.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return nil, errors.New("invalid HTTPS origin")
		}
	}
	base.Path = ""
	base.RawPath = ""
	return base, nil
}

// authorityOrigin validates and canonicalizes an authority HTTPS origin so
// operator overrides cannot transmit credentials over plaintext HTTP or inject
// paths, queries, or fragments.
func authorityOrigin(raw string) (string, error) {
	base, err := parseHTTPSOrigin(raw)
	if err != nil {
		return "", errors.New("invalid authority HTTPS origin")
	}
	return base.String(), nil
}

// loadRelease returns the compiled-in release configuration when path is empty,
// or reads and verifies an explicit release manifest file for operator tools.
func loadRelease(path string) (*networkconfig.Config, error) {
	if path == "" {
		return builtinnetwork.Config("")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxReleaseManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxReleaseManifestBytes {
		return nil, fmt.Errorf("release manifest exceeds %d bytes", maxReleaseManifestBytes)
	}
	return builtinnetwork.Parse(data, "")
}

// resolveAuthority selects an explicit authority override when provided, or
// falls back to the release manifest's authority URL.
func resolveAuthority(config *networkconfig.Config, override string) (string, error) {
	if override != "" {
		return authorityOrigin(override)
	}
	if config == nil || config.Authority == nil || config.Authority.URL == "" {
		return "", errors.New("release does not specify an authority")
	}
	return authorityOrigin(config.Authority.URL)
}
