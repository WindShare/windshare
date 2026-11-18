package nodedirectory

// History authenticates immutable publication history without granting admission.
// Old publications establish alias ownership and rollback barriers even when they
// lack the node identity now required by runtime snapshots.
type History struct{ snapshot Snapshot }

func VerifyHistory(data []byte, network string, roots []Root) (History, error) {
	snapshot, err := verifySigned(data, network, roots)
	if err != nil {
		return History{}, err
	}
	return History{snapshot: snapshot}, nil
}

func (history History) Cursor() Cursor { return history.snapshot.Cursor() }

func (history History) RetainAliases(previous map[string]string) (map[string]string, error) {
	return RetainAliases(previous, history.snapshot)
}
