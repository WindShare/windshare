package icepolicy

import "github.com/windshare/windshare/core/nodedirectory"

const ExistingDefaultSTUNServer = "stun:stun.l.google.com:19302"

const CloudflareSTUNServer = "stun:stun.cloudflare.com:3478"
const TwilioSTUNServer = "stun:global.stun.twilio.com:3478"

// Provider-published endpoints are fallbacks, not mainland reachability claims.
func DefaultPool() ICEEndpointPool {
	pool, _ := NewICEEndpointPool([]Endpoint{
		{ID: "shipped-google-stun", URL: ExistingDefaultSTUNServer, Provider: "google", FailureDomain: "google-stun", Family: "any", Trust: "reviewed", Enabled: true},
		{ID: "public-cloudflare-stun", URL: CloudflareSTUNServer, Provider: "cloudflare", FailureDomain: "cloudflare-stun", Family: "any", Trust: "reviewed", Enabled: true},
		{ID: "public-twilio-stun", URL: TwilioSTUNServer, Provider: "twilio", FailureDomain: "twilio-stun", Family: "any", Trust: "reviewed", Enabled: true},
	})
	return pool
}

// DirectoryPool consumes only an application's currently trusted snapshot.
func DirectoryPool(directory nodedirectory.Payload) ICEEndpointPool {
	entries := DefaultPool().endpoints
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.URL] = true
	}
	add := func(url, region, provider, domain string) {
		if seen[url] || len(entries) >= MaximumCatalogEndpoints {
			return
		}
		seen[url] = true
		entries = append(entries, Endpoint{ID: "directory:" + url, URL: url, Region: region, Provider: provider, FailureDomain: domain, Family: "any", Trust: "reviewed", Enabled: true})
	}
	for _, node := range directory.Nodes {
		if node.Status == nodedirectory.Draining || !node.HasRole("stun") {
			continue
		}
		for _, url := range node.STUN {
			add(url, node.Region, node.Provider, node.Operator+":"+node.Provider+":"+node.Region)
		}
	}
	for _, endpoint := range directory.PublicSTUN {
		add(endpoint.URL, endpoint.Region, endpoint.Provider, endpoint.FailureDomain)
	}
	pool, err := NewICEEndpointPool(entries)
	if err != nil {
		return DefaultPool()
	}
	return pool
}
