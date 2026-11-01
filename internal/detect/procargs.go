package detect

import (
	"bytes"
	"encoding/binary"
)

// maxArgs bounds how many arguments parseProcArgs returns.
const maxArgs = 4096

// pathAlign is the alignment of the executable path's slot in a kern.procargs2 buffer. The
// kernel copies the path with its NUL, then pads the slot with NULs to the pointer size (8 on
// every Mac that runs a 64-bit process) before argv begins.
const pathAlign = 8

// sZomb is p_stat of a macOS zombie (SZOMB in sys/proc.h).
const sZomb = 5

// readsArgs says whether a macOS process with this p_stat has argv to read: a zombie has none.
func readsArgs(stat int8) bool { return stat != sZomb }

// procArgsSlack is how much larger than the reported size each kern.procargs2 read's buffer
// is, per try: the first try, then the one retry.
var procArgsSlack = [2]int{64, 4096}

// readProcArgs reads a process's argv through kern.procargs2 and keeps it only when the reply
// proves a whole copy. size asks the kernel for the args area's size; read reads into buf and
// returns the reply's length. argMax is kern.argmax: the kernel refuses a larger buffer.
//
// The kernel copies the whole area when the buffer holds it, and replies with its length.
// When the buffer is too small, it copies only the tail, the environment, after the current
// argc, and fills the buffer. That happens when the process runs exec between the size query
// and the read, into a program with a larger args area; parsed as it stands, the tail reads
// environment strings as argv. So the buffer is larger than the reported size, and a reply
// shorter than the buffer is a whole copy (procArgsWhole). A reply that fills it may be a
// tail: readProcArgs asks the size again and retries once with more room, then gives no Args.
// It checks no name: the saved path is the one the caller gave (a link, a relative path), and
// p_comm is the resolved file's.
func readProcArgs(size func() (int, error), read func(buf []byte) (int, error), argMax int) []string {
	for _, slack := range procArgsSlack {
		n, err := size()
		if err != nil {
			return nil
		}
		l := min(n+slack, argMax-4)
		if l <= 4 {
			return nil
		}
		buf := make([]byte, l)
		got, err := read(buf)
		if err != nil {
			return nil
		}
		if procArgsWhole(got, l) {
			return parseProcArgs(buf[:got])
		}
	}
	return nil
}

// procArgsWhole says whether a kern.procargs2 reply of n bytes into a buffer of l bytes is the
// whole args area: only a reply shorter than its buffer proves it.
func procArgsWhole(n, l int) bool { return n >= 0 && n < l }

// parseProcArgs reads argv from a macOS kern.procargs2 buffer: argc as a 32-bit little-endian
// integer (every Mac Go builds for is little-endian), the executable path ending in NUL, NUL
// padding to the next multiple of pathAlign, then argc NUL-ended arguments, then the
// environment, which it never reads. A buffer cut short gives the arguments it holds.
//
// It skips NULs only inside the padding, never past it: an empty argument is a NUL of its
// own, so skipping every NUL after the path would eat an empty argv[0] and shift each later
// argument by one, until the first environment string read as the last argument. When the
// padding rule is ever wrong, the error goes the safe way: an extra NUL reads as an empty
// argument, and since it reads at most argc strings, it stops earlier, never in the
// environment.
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
	slot := (i + 1 + pathAlign - 1) / pathAlign * pathAlign
	b = b[i+1:]
	for n := slot - (i + 1); n > 0 && len(b) > 0 && b[0] == 0; n-- {
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
