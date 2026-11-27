package peers

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// maxProofFile bounds the proof file read into memory.
const maxProofFile = 1 << 20

// Proofs is the set of device ids that proved their hab key on a peer link to this hab
// (docs/specs/transport.md 6.3), kept in a file of one id per line in the state directory. A
// device in it must prove on every later link. It implements control.PeerProofs.
type Proofs struct {
	path string
	mu   sync.Mutex
	ids  map[string]bool
}

// OpenProofs reads the proof file at path; a missing file is an empty set.
func OpenProofs(path string) (*Proofs, error) {
	p := &Proofs{path: path, ids: map[string]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxProofFile))
	for sc.Scan() {
		if id := strings.TrimSpace(sc.Text()); id != "" {
			p.ids[id] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("peer proofs %s: %w", path, err)
	}
	return p, nil
}

// Proved reports whether device proved its key here before.
func (p *Proofs) Proved(device string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ids[device]
}

// Record adds device and appends it to the file, which it makes at mode 0600.
func (p *Proofs) Record(device string) error {
	if device == "" || strings.ContainsAny(device, "\r\n") {
		return fmt.Errorf("peer proofs: %q is not a device id", device)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ids[device] {
		return nil
	}
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(device + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	p.ids[device] = true
	return nil
}
