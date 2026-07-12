package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestServerConfigAdvertisesEffectivePolicy(t *testing.T) {
	cfg := ServerConfig{
		ProtocolVersions: []string{ProtocolVersion},
		Limits: ServerLimits{
			MaxFrameSize:             64 << 10,
			MaxManifestSize:          16 << 20,
			MaxSignalingMessageBytes: MaxSignalingMessageBytes,
			MaxHeaderBytes:           1 << 20,
			Admission: AdmissionLimits{
				MaxConnections:            100,
				MaxConcurrentShares:       20,
				MaxSharesPerSource:        3,
				MaxManifestBytesPerSource: 1024,
				MaxTotalManifestBytes:     4096,
				RegisterPerSource:         RateLimit{PerSecond: 0.5, Burst: 2},
				JoinPerSource:             RateLimit{PerSecond: 2, Burst: 4},
				JoinPerShare:              RateLimit{PerSecond: 1, Burst: 3},
			},
			Timeouts: ServerTimeouts{
				HTTPReadHeaderMilliseconds:       5000,
				HTTPReadMilliseconds:             15000,
				HTTPIdleMilliseconds:             60000,
				WebSocketRoleMilliseconds:        15000,
				WebSocketKeepaliveMilliseconds:   60000,
				SenderReconnectGraceMilliseconds: 30000,
			},
		},
	}
	wire, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"protocolVersions":["v1"]`,
		`"maxConnections":100`,
		`"maxSharesPerSource":3`,
		`"registerPerSource":{"perSecond":0.5,"burst":2}`,
		`"webSocketKeepaliveMilliseconds":60000`,
		`"senderReconnectGraceMilliseconds":30000`,
	} {
		if !strings.Contains(string(wire), field) {
			t.Fatalf("config JSON %s does not contain %s", wire, field)
		}
	}
	var roundTrip ServerConfig
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip, cfg) {
		t.Fatalf("round trip = %+v, want %+v", roundTrip, cfg)
	}
}
