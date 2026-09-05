// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package webrtc

import (
	"github.com/pion/ice/v4"
	"slices"
)

// SetICEProviderConfig installs one immutable provider capability snapshot.
// This setting adds capabilities to the normal PeerConnection ICE gatherer;
// it does not construct a second ICE agent or alter SDP after gathering.
func (e *SettingEngine) SetICEProviderConfig(config ice.ProviderConfig) {
	config.MappedUDPEndpoints = slices.Clone(config.MappedUDPEndpoints)
	config.MappedTCPEndpoints = slices.Clone(config.MappedTCPEndpoints)
	e.iceProviderConfig = config
}
