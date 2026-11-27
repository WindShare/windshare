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

// EvaluateEligibility derives current community eligibility from previously
// recorded refusals, the active configuration, and any fresh owner observations.
// Only an explicit signed refusal is sticky across offline periods; a later
// reachable observation in any other state releases the refusal and its notice.
func EvaluateEligibility(current Eligibility, c Config, observed map[string]serviceauthority.PublicRelay) Eligibility {
	disabled := map[string]bool{}
	owned := c.OwnedNative()
	for _, p := range owned {
		k := p.Node.NodeKey
		if s := observed[k]; s == serviceauthority.RelayDisabled || s == serviceauthority.RelayUnknown && current.Disabled[k] {
			disabled[k] = true
		}
	}
	next := Eligibility{
		Disabled: disabled,
		Paused:   len(owned) > 0,
		Notified: current.Notified,
	}
	for _, p := range owned {
		if !disabled[p.Node.NodeKey] {
			next.Paused = false
		}
	}
	if !next.Paused {
		next.Notified = false
	}
	return next
}

func ReconcileEligibility(tx *statefile.Transaction, c Config, observed map[string]serviceauthority.PublicRelay) (Eligibility, error) {
	e, err := ReadEligibility(tx)
	if err != nil {
		return e, err
	}
	previous, _ := json.Marshal(e)
	e = EvaluateEligibility(e, c, observed)
	next, _ := json.Marshal(e)
	if string(next) != string(previous) {
		err = tx.Put(statefile.CommunityEligibility, next)
	}
	return e, err
}

// ReadCommunityState reads saved settings and derives current community
// eligibility without acquiring a write lock unless commit recovery is pending.
func (s FileStore) ReadCommunityState(ctx context.Context) (Config, Eligibility, error) {
	path, err := s.Location()
	if err != nil {
		return Config{}, Eligibility{}, err
	}
	var c Config
	var e Eligibility
	err = statefile.Read(ctx, path, func(tx *statefile.Transaction) error {
		var err error
		c, err = ReadSettings(tx)
		if err != nil {
			return err
		}
		stored, err := ReadEligibility(tx)
		if err != nil {
			return err
		}
		e = EvaluateEligibility(stored, c, nil)
		return nil
	})
	return c, e, err
}

func (s FileStore) TakeCommunityNotice(ctx context.Context) (bool, error) {
	// Avoid opening or locking config.json.lock when there is no undelivered notice.
	_, current, err := s.ReadCommunityState(ctx)
	if err != nil || !current.Paused || current.Notified {
		return false, err
	}
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
