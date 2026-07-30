package browsermatrixbroker

import (
	"sync"
	"time"
)

type acquisitionClaim struct {
	operation        sync.Mutex
	scope            requestScope
	leaseID          string
	providerLeaseID  string
	turnCredentialID string
	turnUsername     string
	turnExpiresAt    string
	controlOwned     bool
	providerOwned    bool
	failed           bool
	ready            bool
	inFlight         int
	expiresAt        time.Time
}

type compositeLease struct {
	operation         sync.Mutex
	owner             *acquisitionClaim
	scope             requestScope
	leaseID           string
	authorityID       string
	attestationSHA256 string
	controlExpiresAt  string
	controlExpires    time.Time
	providerLeaseID   string
	turnCredentialID  string
	turnUsername      string
	turnExpiresAt     string
	controlSettled    bool
	turnSettled       bool
	ready             bool
	terminal          bool
	terminalOperation string
	receipt           ReceiptPayload
	expiresAt         time.Time
}

type retirementClaim struct {
	scope      requestScope
	done       chan struct{}
	receipt    ReceiptPayload
	err        error
	expiresAt  time.Time
	completed  bool
	inProgress bool
}
