package processrun

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	identityEntropyBytes = 16
	commandScenario      = "perfevidence-command"
)

func NewIdentity() (protocol.Identity, error) {
	entropy := make([]byte, identityEntropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return protocol.Identity{}, fmt.Errorf("generate owned-command operation identity: %w", err)
	}
	identity := protocol.Identity{
		RunID:       fmt.Sprintf("perfevidence-%d", os.Getpid()),
		OperationID: "operation-" + hex.EncodeToString(entropy),
		Scenario:    commandScenario,
	}
	if err := protocol.ValidateIdentity(identity); err != nil {
		return protocol.Identity{}, fmt.Errorf("validate owned-command operation identity: %w", err)
	}
	return identity, nil
}
