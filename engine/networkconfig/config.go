// Package networkconfig describes application-injected network and frontend trust.
package networkconfig

import (
	"time"

	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/networkdirectory"
)

type Config struct {
	CommunitySettingsPath    string
	CommunityPaused          func()
	BootstrapNodes           []string
	Authority                *account.Config
	Directory                networkdirectory.Config
	LinkHost                 string
	BackupEntry              string
	GlobalPairCoversMainland bool
}

type Candidate struct {
	Source        string
	URL           string
	Alias         string
	Configured    bool
	CooldownUntil time.Time
	ServerRetryAt time.Time
}

type Filter struct{ Endpoint, Reason string }
type Decision struct {
	DirectoryGeneration uint64
	DirectorySerial     uint64
	NetworkGeneration   string
	ConfiguredOnly      bool
	CooldownFallback    bool
	Candidates          []Candidate
	Filters             []Filter
	FrontendReason      string
}
