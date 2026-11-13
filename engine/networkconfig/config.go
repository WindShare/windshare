// Package networkconfig describes application-injected network and frontend trust.
package networkconfig

import "github.com/windshare/windshare/engine/networkdirectory"

type Config struct {
	Directory                networkdirectory.Config
	LinkHost                 string
	BackupEntry              string
	GlobalPairCoversMainland bool
}

type Candidate struct {
	URL        string
	Alias      string
	Configured bool
}

type Filter struct{ Endpoint, Reason string }
type Decision struct {
	DirectoryGeneration uint64
	DirectorySerial     uint64
	NetworkGeneration   string
	ConfiguredOnly      bool
	Candidates          []Candidate
	Filters             []Filter
	FrontendReason      string
}
