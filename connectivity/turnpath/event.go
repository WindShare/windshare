package turnpath

import (
	"time"

	"github.com/windshare/windshare/core/connectionpath"
)

type Event struct {
	At          time.Time
	Subject     Subject
	Provider    string
	Account     string
	Profile     string
	Milestone   connectionpath.Milestone
	Reason      string
	UsefulBytes uint64
}
