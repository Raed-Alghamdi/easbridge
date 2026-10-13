package mimestruct

import (
	"bufio"
	"bytes"
)

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }
