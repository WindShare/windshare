//go:build !windshare_development

// Package network embeds the release's shared native/browser trust configuration.
package network

import _ "embed"

//go:embed release.json
var Release []byte
