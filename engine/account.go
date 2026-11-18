package engine

import "github.com/windshare/windshare/engine/account"

// Accounts is shared by all official-node registrations owned by this engine.
// Existing relay registrations keep their original attribution after login.
func (engine *Engine) Accounts() *account.Service { return engine.network.Accounts() }
