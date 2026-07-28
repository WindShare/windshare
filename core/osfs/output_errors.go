package osfs

import (
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/pathfailure"
)

var (
	ErrOutOfRange  = outputfault.ErrOutOfRange
	ErrPathEscape  = outputfault.ErrPathEscape
	ErrPathTooLong = pathfailure.ErrTooLong
)
