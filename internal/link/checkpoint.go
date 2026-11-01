package link

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/cmdmarks"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/progstatus"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// checkpointVersion is the format version of a checkpoint file. Version 2 added the snapshot
// length and CRC32.
const checkpointVersion = 2

// checkpointSuffix ends the name of every checkpoint file: <pane uuid>.ckpt.
const checkpointSuffix = ".ckpt"

// checkpoint is one pane's saved screen: a parser snapshot and the absolute output offset it
// covers (spec 2.3). On disk it is one JSON header line, then the raw snapshot bytes.
type checkpoint struct {
	Version  int         `json:"v"`
	Pane     string      `json:"pane"`
	Holder   string      `json:"holder"`
	Offset   uint64      `json:"offset"`
	Encoding vt.Encoding `json:"encoding"`
	Cols     int         `json:"cols"`
	Rows     int         `json:"rows"`
	Suspect  bool        `json:"suspect,omitempty"`
	Written  time.Time   `json:"written"`
	// Len and CRC are the snapshot's length and CRC-32 (IEEE). A file that disagrees with them
	// counts as no checkpoint.
	Len int    `json:"len"`
	CRC uint32 `json:"crc32"`
	// Commands is the pane's command list and mark state at Offset (hab-logic-api.md 2.5).
	// Absent in a checkpoint from an older build: the list then starts empty.
	Commands *cmdmarks.Saved `json:"commands,omitempty"`
	// Report is the pane's newest root status report (OSC 7501) before Offset, unless a shell
	// prompt came after it. A restart replays from Offset, so the bytes that held it are not
	// replayed; gff-hab hands it on like a replayed report. Absent in an older checkpoint.
	Report   *progstatus.Report `json:"report,omitempty"`
	Snapshot []byte             `json:"-"`
}

func checkpointPath(dir string, u hp.UUID) string {
	return filepath.Join(dir, u.String()+checkpointSuffix)
}

// writeCheckpoint writes c atomically (writeAtomic). A crash leaves the old checkpoint or the new
// one, never half.
func writeCheckpoint(dir string, u hp.UUID, c *checkpoint) error {
	c.Len, c.CRC = len(c.Snapshot), crc32.ChecksumIEEE(c.Snapshot)
	hdr, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeAtomicParts(dir, u.String()+checkpointSuffix, hdr, []byte{'\n'}, c.Snapshot)
}

// writeAtomic writes data to dir/name atomically: a temporary file in the same directory,
// synced, then renamed over the old file, and the directory synced.
func writeAtomic(dir, name string, data []byte) error {
	return writeAtomicParts(dir, name, data)
}

func writeAtomicParts(dir, name string, parts ...[]byte) error {
	f, err := os.CreateTemp(dir, ".tmp-"+name+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	w := bufio.NewWriter(f)
	for _, p := range parts {
		w.Write(p)
	}
	err = w.Flush()
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// maxCheckpointHeader is the longest checkpoint header line readCheckpoint reads. The largest
// header gff-hab writes is about cmdmarks.MaxCommands commands, each with cmdmarks.MaxText bytes
// of text that JSON may escape to 6 bytes a byte: under 7 MiB. A longer line is corrupt or
// hostile, and is not read.
const maxCheckpointHeader = 8 << 20

// readCheckpoint reads pane u's checkpoint. It returns os.ErrNotExist when there is none. It
// reads at most maxCheckpointHeader bytes of header, and a snapshot only of the length the
// header gives when the file is exactly that long.
func readCheckpoint(dir string, u hp.UUID) (*checkpoint, error) {
	f, err := os.Open(checkpointPath(dir, u))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	hdr, err := bufio.NewReader(io.LimitReader(f, maxCheckpointHeader+1)).ReadBytes('\n')
	switch {
	case err == io.EOF && len(hdr) > maxCheckpointHeader:
		return nil, fmt.Errorf("checkpoint header is over %d bytes", maxCheckpointHeader)
	case err == io.EOF:
		return nil, errors.New("checkpoint has no header line")
	case err != nil:
		return nil, err
	}
	var c checkpoint
	if err := json.Unmarshal(hdr, &c); err != nil {
		return nil, fmt.Errorf("checkpoint header: %w", err)
	}
	if c.Version != checkpointVersion {
		return nil, fmt.Errorf("checkpoint version %d, want %d", c.Version, checkpointVersion)
	}
	if c.Pane != u.String() {
		return nil, fmt.Errorf("checkpoint is for pane %s", c.Pane)
	}
	if c.Cols <= 0 || c.Rows <= 0 {
		return nil, fmt.Errorf("checkpoint size %dx%d", c.Cols, c.Rows)
	}
	if n := st.Size() - int64(len(hdr)); c.Len < 0 || n != int64(c.Len) {
		return nil, fmt.Errorf("checkpoint snapshot is %d bytes, header says %d", n, c.Len)
	}
	snap := make([]byte, c.Len)
	if _, err := f.ReadAt(snap, int64(len(hdr))); err != nil && !(err == io.EOF && c.Len == 0) {
		return nil, fmt.Errorf("checkpoint snapshot: %w", err)
	}
	if sum := crc32.ChecksumIEEE(snap); sum != c.CRC {
		return nil, fmt.Errorf("checkpoint snapshot CRC %08x, header says %08x", sum, c.CRC)
	}
	c.Snapshot = snap
	return &c, nil
}

func removeCheckpoint(dir string, u hp.UUID) {
	os.Remove(checkpointPath(dir, u))
}

// removeTemps removes temporary files a crash left behind.
func removeTemps(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// removeCheckpointsExcept removes every checkpoint file whose pane is not in keep (pane uuid
// strings). It returns the panes it removed.
func removeCheckpointsExcept(dir string, keep map[string]bool) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), checkpointSuffix)
		if !ok || strings.HasPrefix(name, ".") || keep[name] {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed = append(removed, name)
		}
	}
	return removed
}
