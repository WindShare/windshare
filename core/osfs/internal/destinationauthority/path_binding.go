package destinationauthority

import (
	"errors"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrInvalidArtifactPath = errors.New("destination authority artifact path is invalid")

// PhysicalArtifactPath applies exactly one requested-to-physical alias. It does
// not parse source coordinates; only the projector's logical artifact output is
// eligible to reach this boundary.
func PhysicalArtifactPath(logicalPath string, reservation ReservedEntry) (string, error) {
	canonical, err := catalog.CanonicalPath(logicalPath)
	if err != nil || canonical != logicalPath || !reservation.Valid() {
		return "", ErrInvalidArtifactPath
	}
	components := strings.Split(logicalPath, "/")
	if components[0] != reservation.requestedName {
		return "", ErrInvalidArtifactPath
	}
	if reservation.kind == receivecontract.ContainerEntryResultRoot {
		if len(components) == 1 {
			return "", nil
		}
		return strings.Join(components[1:], "/"), nil
	}
	components[0] = reservation.physicalName
	return strings.Join(components, "/"), nil
}
