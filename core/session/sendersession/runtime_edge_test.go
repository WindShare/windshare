package sendersession

import (
	"io"
)

type edgeErrorReader struct{}

func (edgeErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
