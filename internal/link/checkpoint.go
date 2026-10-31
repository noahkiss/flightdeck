package link

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// checkpointVersion is the format version of a checkpoint file.
const checkpointVersion = 1

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
	Snapshot []byte      `json:"-"`
}

func checkpointPath(dir string, u hp.UUID) string {
	return filepath.Join(dir, u.String()+checkpointSuffix)
}

// writeCheckpoint writes c atomically: a temporary file in the same directory, synced, then
// renamed over the old checkpoint. A crash leaves the old checkpoint or the new one, never half.
func writeCheckpoint(dir string, u hp.UUID, c *checkpoint) error {
	hdr, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+u.String()+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	w := bufio.NewWriter(f)
	w.Write(hdr)
	w.WriteByte('\n')
	w.Write(c.Snapshot)
	err = w.Flush()
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, checkpointPath(dir, u))
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// readCheckpoint reads pane u's checkpoint. It returns os.ErrNotExist when there is none.
func readCheckpoint(dir string, u hp.UUID) (*checkpoint, error) {
	b, err := os.ReadFile(checkpointPath(dir, u))
	if err != nil {
		return nil, err
	}
	hdr, snap, ok := bytes.Cut(b, []byte{'\n'})
	if !ok {
		return nil, errors.New("checkpoint has no header line")
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
