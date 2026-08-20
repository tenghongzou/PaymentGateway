package app_test

import (
	"bytes"
	"io"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
