package main

import (
	"encoding/json"
	"net/http"

	pion "github.com/pion/webrtc/v4"
)

func (p *probe) stats(w http.ResponseWriter, r *http.Request) {
	connection := p.path(r)
	if connection == nil {
		http.NotFound(w, r)
		return
	}
	pair, selected := connection.SelectedPair()
	resources, _ := connection.TURNResources()
	var localCandidate *pion.ICECandidateStats
	if selected {
		for _, value := range connection.GetStats() {
			candidate, ok := value.(pion.ICECandidateStats)
			if ok && candidate.Type == pion.StatsTypeLocalCandidate &&
				candidate.IP == pair.LocalAddress && candidate.Port == int32(pair.LocalPort) {
				localCandidate = &candidate
				break
			}
		}
	}
	// A relay ICE pair is UDP even when the client-to-TURN hop uses TCP/TLS.
	// Preserve the actual control transport so unlike paths are not compared as UDP.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pair": pair, "selected": selected, "resources": resources, "localCandidate": localCandidate,
		"sctp": connection.SCTP().Stats(), "bufferedBytes": connection.SCTP().BufferedAmount(),
	})
}
