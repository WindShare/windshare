package icepolicy

import "github.com/windshare/windshare/core/nodedirectory"

const ExistingDefaultSTUNServer = "stun:stun.l.google.com:19302"

const CloudflareSTUNServer = "stun:stun.cloudflare.com:3478"
const TwilioSTUNServer = "stun:global.stun.twilio.com:3478"

// Static priority tiers keep healthy active directory nodes ahead of canary
// and draining nodes, signed public STUN, and embedded fallbacks, while
// allowing runtime profile success to override static order.
const (
	DirectoryNodeSTUNPriority     = 4
	DirectoryCanarySTUNPriority   = 3
	DirectoryDrainingSTUNPriority = 2
	DirectoryPublicSTUNPriority   = 1
	FallbackSTUNPriority          = 0
)

func directoryNodeSTUNPriority(status nodedirectory.Status) int {
	switch status {
	case nodedirectory.Active:
		return DirectoryNodeSTUNPriority
	case nodedirectory.Canary:
		return DirectoryCanarySTUNPriority
	default:
		return DirectoryDrainingSTUNPriority
	}
}

// Provider-published endpoints are fallbacks, not mainland reachability claims.
func DefaultPool() ICEEndpointPool {
	pool, _ := NewICEEndpointPool([]Endpoint{
		{ID: "shipped-google-stun", URL: ExistingDefaultSTUNServer, Provider: "google", FailureDomain: "google-stun", Family: "any", Trust: "reviewed", Priority: FallbackSTUNPriority, Enabled: true},
		{ID: "public-cloudflare-stun", URL: CloudflareSTUNServer, Provider: "cloudflare", FailureDomain: "cloudflare-stun", Family: "any", Trust: "reviewed", Priority: FallbackSTUNPriority, Enabled: true},
		{ID: "public-twilio-stun", URL: TwilioSTUNServer, Provider: "twilio", FailureDomain: "twilio-stun", Family: "any", Trust: "reviewed", Priority: FallbackSTUNPriority, Enabled: true},
	})
	return pool
}

func (pool ICEEndpointPool) WithCommunity(entries []Endpoint) ICEEndpointPool {
	all := append([]Endpoint(nil), pool.endpoints...)
	seen := map[string]bool{}
	for _, e := range all {
		seen[e.URL] = true
	}
	for _, e := range entries {
		if len(all) >= MaximumCatalogEndpoints {
			break
		}
		if !seen[e.URL] && e.Trust == "community" {
			all = append(all, e)
			seen[e.URL] = true
		}
	}
	next, err := NewICEEndpointPool(all)
	if err != nil {
		return pool
	}
	return next
}

// DirectoryPool consumes only an application's currently trusted snapshot.
// Directory entries are deduplicated ahead of fallbacks so signed metadata and
// priority win when a URL also appears in the embedded fallback set.
func DirectoryPool(directory nodedirectory.Payload) ICEEndpointPool {
	var entries []Endpoint
	seen := map[string]bool{}
	add := func(entry Endpoint) {
		if seen[entry.URL] || len(entries) >= MaximumCatalogEndpoints {
			return
		}
		seen[entry.URL] = true
		entries = append(entries, entry)
	}
	for _, node := range directory.Nodes {
		if !node.HasRole("stun") {
			continue
		}
		priority := directoryNodeSTUNPriority(node.Status)
		for _, url := range node.STUN {
			add(Endpoint{
				ID: "directory:" + url, URL: url, Region: node.Region, Provider: node.Provider,
				FailureDomain: node.Operator + ":" + node.Provider + ":" + node.Region,
				Family:        "any", Trust: "reviewed", Priority: priority, Enabled: true,
			})
		}
	}
	for _, endpoint := range directory.PublicSTUN {
		add(Endpoint{
			ID: "directory:" + endpoint.URL, URL: endpoint.URL, Region: endpoint.Region, Provider: endpoint.Provider,
			FailureDomain: endpoint.FailureDomain, Family: "any", Trust: "reviewed",
			Priority: DirectoryPublicSTUNPriority, Enabled: true,
		})
	}
	for _, fallback := range DefaultPool().endpoints {
		add(fallback)
	}
	pool, err := NewICEEndpointPool(entries)
	if err != nil {
		return DefaultPool()
	}
	return pool
}
