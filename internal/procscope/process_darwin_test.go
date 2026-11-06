package procscope

import (
	"encoding/binary"
	"testing"

	"github.com/noahkiss/flightdeck/internal/hook"
)

// The path slot is padded, but empty argv entries must remain distinct from padding.
func TestParseEnvironment(t *testing.T) {
	token := "scope-token"
	env := "GFF_HAB=" + hook.Addr("/socket", token) + "\x00"
	for _, path := range []string{"/bin/x", "/a-longer-path"} {
		for _, args := range [][]string{{"x"}, {"", "x"}, {"x", "", "GFF_HAB=" + hook.Addr("/socket", token)}} {
			b := make([]byte, 4)
			binary.LittleEndian.PutUint32(b, uint32(len(args)))
			slot := append([]byte(path), 0)
			for len(slot)%8 != 0 {
				slot = append(slot, 0)
			}
			b = append(b, slot...)
			for _, a := range args {
				b = append(b, []byte(a)...)
				b = append(b, 0)
			}
			if got := parseEnvironment(b); matchesEnv(got, token) {
				t.Fatal("argv was read as environment")
			}
			b = append(b, []byte(env)...)
			if got := string(parseEnvironment(b)); got != env {
				t.Fatalf("path %q args %q: env %q", path, args, got)
			}
		}
	}
}
