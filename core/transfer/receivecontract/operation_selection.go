package receivecontract

// OperationSelection distinguishes an automatic continuation target from an
// explicitly selected task. Redownload has no receipt or partial-data parent.
type OperationSelection struct {
	kind OperationSelectionKind
	id   OperationID
}
type OperationSelectionKind uint8

const (
	OperationAutomatic OperationSelectionKind = iota
	OperationRedownload
	OperationByID
)

func AutomaticOperation() OperationSelection  { return OperationSelection{} }
func RedownloadOperation() OperationSelection { return OperationSelection{kind: OperationRedownload} }
func SelectOperationID(id OperationID) (OperationSelection, error) {
	if id.IsZero() {
		return OperationSelection{}, ErrInvalidReceiveContract
	}
	return OperationSelection{kind: OperationByID, id: id}, nil
}
func (s OperationSelection) Kind() OperationSelectionKind { return s.kind }
func (s OperationSelection) ID() OperationID              { return s.id }
func (s OperationSelection) Valid() bool {
	return (s.kind == OperationAutomatic || s.kind == OperationRedownload) && s.id.IsZero() || s.kind == OperationByID && !s.id.IsZero()
}
