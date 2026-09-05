package main

import (
	"flag"
	"strings"

	"github.com/windshare/windshare/relay/stunonly"
)

const (
	defaultSTUNAddress      = ":3478"
	defaultSTUNAdminAddress = "127.0.0.1:8081"
)

type stunFlags struct {
	udp, admin             *string
	total, source, sources *int
}

func registerSTUNFlags(flags *flag.FlagSet) stunFlags {
	return stunFlags{
		udp:     flags.String("stun-udp", defaultSTUNAddress, "comma-separated UDP STUN Binding listeners; empty disables STUN"),
		admin:   flags.String("stun-admin", defaultSTUNAdminAddress, "private STUN health and metrics HTTP address; empty disables admin"),
		total:   flags.Int("stun-requests-per-second", stunonly.DefaultRequestsPerSecond, "STUN per-listener request limit"),
		source:  flags.Int("stun-source-requests-per-second", stunonly.DefaultSourceRequestsPerSecond, "STUN per-listener source IP request limit"),
		sources: flags.Int("stun-max-sources", stunonly.DefaultMaximumSources, "STUN per-listener tracked source IP limit"),
	}
}

func (f stunFlags) config() stunonly.ServiceConfig {
	config := stunonly.ServiceConfig{AdminAddress: *f.admin, Limits: stunonly.Config{
		RequestsPerSecond: *f.total, SourceRequestsPerSecond: *f.source, MaximumSources: *f.sources,
	}}
	for address := range strings.SplitSeq(*f.udp, ",") {
		if address = strings.TrimSpace(address); address != "" {
			config.UDPAddresses = append(config.UDPAddresses, address)
		}
	}
	return config
}
