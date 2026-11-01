package detect

import (
	"bytes"
	"encoding/binary"
)

// maxArgs bounds how many arguments parseProcArgs returns.
const maxArgs = 4096

// parseProcArgs reads argv from a macOS kern.procargs2 buffer: argc as a 32-bit little-endian
// integer (every Mac Go builds for is little-endian), the executable path ending in NUL, NUL
// padding, then argc NUL-ended arguments, then the environment, which it never reads. A buffer
// cut short gives the arguments it holds. An empty argv[0] reads as padding and is lost.
func parseProcArgs(b []byte) []string {
	if len(b) < 4 {
		return nil
	}
	argc := int(int32(binary.LittleEndian.Uint32(b)))
	b = b[4:]
	if argc <= 0 {
		return nil
	}
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return nil
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	var args []string
	for len(args) < min(argc, maxArgs) && len(b) > 0 {
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			args = append(args, string(b))
			break
		}
		args = append(args, string(b[:j]))
		b = b[j+1:]
	}
	return args
}
