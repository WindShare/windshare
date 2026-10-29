package outputportable

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// Directory creation metadata is best-effort evidence scoped by the retained
// task namespace and path, not a unique object ID. Persisting dev/ino or FAT
// directory-entry indices would instead reject the same directories after a
// remount. Live path guards still compare currently open native objects.
func creationStamp(seconds int64, nanoseconds int64) ([]byte, error) {
	if nanoseconds < 0 || nanoseconds >= int64(time.Second) {
		return nil, fmt.Errorf("%w: invalid creation timestamp", outputcap.ErrUnsafeNamespace)
	}
	stamp := binary.BigEndian.AppendUint64(nil, uint64(seconds))
	return binary.BigEndian.AppendUint32(stamp, uint32(nanoseconds)), nil
}
