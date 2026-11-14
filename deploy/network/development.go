//go:build windshare_development

package network

import _ "embed"

//go:embed development/release.json
var Release []byte
