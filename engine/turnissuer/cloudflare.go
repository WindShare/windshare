package turnissuer

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

type cloudflareServer struct {
	URLs                 []string
	Username, Credential string
}

// Cloudflare can encode both iceServers and each server's urls as a single item or an array.
func cloudflareItems(data json.RawMessage) []json.RawMessage {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if data[0] != '[' {
		return []json.RawMessage{data}
	}
	var items []json.RawMessage
	if json.Unmarshal(data, &items) != nil {
		return nil
	}
	return items
}

func selectCloudflareServers(response json.RawMessage, filters []string) (cloudflareServer, error) {
	var selected cloudflareServer
	for _, server := range cloudflareItems(response) {
		var entry struct {
			URLs                 json.RawMessage
			Username, Credential string
		}
		if json.Unmarshal(server, &entry) != nil || !p.ValidTURNCredential(entry.Username) || !p.ValidTURNCredential(entry.Credential) {
			continue
		}
		// A grant carries one credential pair, even when its transports arrived in separate entries.
		if len(selected.URLs) != 0 && (entry.Username != selected.Username || entry.Credential != selected.Credential) {
			continue
		}
		for _, rawURL := range cloudflareItems(entry.URLs) {
			var url string
			if json.Unmarshal(rawURL, &url) != nil || !p.ValidTURNURL(url) || slices.Contains(selected.URLs, url) {
				continue
			}
			if len(filters) != 0 && !slices.ContainsFunc(filters, func(filter string) bool {
				return url == filter || strings.HasPrefix(url, filter+"?")
			}) {
				continue
			}
			selected.Username, selected.Credential = entry.Username, entry.Credential
			selected.URLs = append(selected.URLs, url)
			if len(selected.URLs) == p.MaxURLsPerGrant {
				return selected, nil
			}
		}
	}
	if len(selected.URLs) == 0 {
		return selected, errUnavailable
	}
	return selected, nil
}
