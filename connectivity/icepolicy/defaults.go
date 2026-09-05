package icepolicy

const ExistingDefaultSTUNServer = "stun:stun.l.google.com:19302"

// DefaultPool preserves the repository's shipped endpoint. Geography and
// infrastructure diversity remain unknown; this is not a reachability claim.
func DefaultPool() ICEEndpointPool {
	pool, _ := NewICEEndpointPool([]Endpoint{{
		ID: "shipped-google-stun", URL: ExistingDefaultSTUNServer,
		Family: "any", Trust: "reviewed", Enabled: true,
	}})
	return pool
}
