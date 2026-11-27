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

// OpenProofs reads the proof file at path; a missing file is an empty set. A file it cannot
// read in full does not stop the hab: logf says so, and the ids read before the problem are
// kept. A device whose id was lost links unproven again until it proves (transport.md 6.3).
// logf may be nil.
func OpenProofs(path string, logf func(string, ...any)) *Proofs {
	p := &Proofs{path: path, ids: map[string]bool{}}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return p
	}
	if err != nil {
		logf("peer proofs %s: %v; no proven device is remembered until it proves again", path, err)
		return p
	}
	defer f.Close()
	lr := &io.LimitedReader{R: f, N: maxProofFile}
	sc := bufio.NewScanner(lr)
	// A line may be as long as the whole file: a long line is a bad id, not a read error.
	sc.Buffer(make([]byte, 0, 64<<10), maxProofFile+1)
	for sc.Scan() {
		if id := strings.TrimSpace(sc.Text()); id != "" {
			p.ids[id] = true
		}
	}
	if err := sc.Err(); err != nil {
		logf("peer proofs %s: %v; kept the %d id(s) read before it", path, err, len(p.ids))
	} else if lr.N == 0 {
		logf("peer proofs %s: the file is over %d bytes; kept the %d id(s) in the first %d", path, maxProofFile, len(p.ids), maxProofFile)
	}
	return p
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
