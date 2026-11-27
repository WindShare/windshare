package relayconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/relayprofile"
)

func (c Config) OwnedNative() []relayprofile.Candidate {
	var owned []relayprofile.Candidate
	for _, p := range c.Profiles {
		if p.Role == relayprofile.Owner && p.Node.Platform == "native" && p.Node.NodeKey != "" && p.Node.Authority == "" {
			owned = append(owned, p)
		}
	}
	return owned
}

type Eligibility struct {
	Disabled map[string]bool `json:"disabled,omitempty"`
	Paused   bool            `json:"paused,omitempty"`
	Notified bool            `json:"notified,omitempty"`
}

func ReadEligibility(tx *statefile.Transaction) (Eligibility, error) {
	data, err := tx.Read(statefile.CommunityEligibility)
	if errors.Is(err, os.ErrNotExist) {
		return Eligibility{}, nil
	}
	if err != nil {
		return Eligibility{}, err
	}
	var e Eligibility
	err = json.Unmarshal(data, &e)
	return e, err
}

// Only an explicit signed refusal is sticky across offline periods. A later
// reachable observation in any other state releases the refusal and its notice.
func ReconcileEligibility(tx *statefile.Transaction, c Config, observed map[string]serviceauthority.PublicRelay) (Eligibility, error) {
	e, err := ReadEligibility(tx)
	if err != nil {
		return e, err
	}
	previous, _ := json.Marshal(e)
	disabled := map[string]bool{}
	owned := c.OwnedNative()
	for _, p := range owned {
		k := p.Node.NodeKey
		if s := observed[k]; s == serviceauthority.RelayDisabled || s == serviceauthority.RelayUnknown && e.Disabled[k] {
			disabled[k] = true
		}
	}
	e.Disabled = disabled
	e.Paused = len(owned) > 0
	for _, p := range owned {
		if !disabled[p.Node.NodeKey] {
			e.Paused = false
		}
	}
	if !e.Paused {
		e.Notified = false
	}
	next, _ := json.Marshal(e)
	if string(next) != string(previous) {
		err = tx.Put(statefile.CommunityEligibility, next)
	}
	return e, err
}

func (s FileStore) TakeCommunityNotice(ctx context.Context) (bool, error) {
	path, err := s.Location()
	if err != nil {
		return false, err
	}
	var show bool
	err = statefile.Transact(ctx, path, func(tx *statefile.Transaction) error {
		c, err := ReadSettings(tx)
		if err != nil {
			return err
		}
		e, err := ReconcileEligibility(tx, c, nil)
		if err != nil || !e.Paused || e.Notified {
			return err
		}
		e.Notified, show = true, true
		data, _ := json.Marshal(e)
		return tx.Put(statefile.CommunityEligibility, data)
	})
	return show, err
}
