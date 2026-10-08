package config

import (
	"bytes"
	"io"
)

var errEOF = io.EOF

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
