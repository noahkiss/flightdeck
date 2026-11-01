// Package peers keeps a hab's peer list and its links to those peers (docs/specs/mesh.md 3, 4).
//
// The peer list is a file the user writes: $GFF_HAB_PEERS, else gff/peers.toml in the user
// config directory. Its [[peers]] entries have the shape of gff's [[hosts]] (internal/hosts): a
// label, one or more endpoints tried in order, and an optional pinned device id. One file can
// serve every host: a hab skips the entry pinned to its own device id, and an entry whose
// endpoint answers with its own device id.
//
// The Manager keeps one control connection per peer through internal/hosts, so a peer that is
// down is offline, never an error, and comes online when its hab answers. The first link pins
// the peer's device id in the file. A different id later is refused until Accept.
package peers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/internal/hosts"
)

// table is the TOML array of tables that holds the peers.
const table = "peers"

// fileConfig is the peer file.
type fileConfig struct {
	Peers []hosts.Entry `toml:"peers"`
}

// Path is the peer file: $GFF_HAB_PEERS, else gff/peers.toml under the user config directory
// on macOS, under $XDG_CONFIG_HOME or ~/.config elsewhere (mesh.md 3.1). It sits beside gff's
// config.toml.
func Path(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if p := getenv("GFF_HAB_PEERS"); p != "" {
		return p, nil
	}
	if goos != "darwin" {
		if x := getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, "gff", "peers.toml"), nil
		}
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	if goos == "darwin" {
		return filepath.Join(h, "Library", "Application Support", "gff", "peers.toml"), nil
	}
	return filepath.Join(h, ".config", "gff", "peers.toml"), nil
}

// DefaultPath is Path for this process.
func DefaultPath() (string, error) {
	return Path(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// Load reads the peer file. A missing file is no peers and no error. Bad entries are reported
// in problems, by line, and the good entries still load (mesh.md 3.1).
func Load(path string) (specs []hosts.Spec, problems []string, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return parse(path, b)
}

func parse(path string, b []byte) (specs []hosts.Spec, problems []string, err error) {
	var fc fileConfig
	md, err := toml.Decode(string(b), &fc)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, k := range md.Undecoded() {
		problems = append(problems, fmt.Sprintf("%s: unknown key %s", path, k))
	}
	specs, more := hosts.ParseEntries(path, "peer", fc.Peers, hosts.HeaderLines(b, table))
	return specs, append(problems, more...), nil
}

// SetPin writes id as the device id of the peer labelled label, and keeps every other line of
// the file as it was, comments included. It replaces the entry's device_id line, or adds one
// after its label line. It refuses a file whose entry it cannot find on lines of their own:
// the user then edits the file by hand. The new file replaces the old one in one rename, with
// the old one's mode.
func SetPin(path, label, id string) error {
	if strings.ContainsAny(id, "\"\\\n") {
		return fmt.Errorf("peers: device id %q is not a plain string", id)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("peers: %w", err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	start, end, labelLine, err := findEntry(lines, label)
	if err != nil {
		return fmt.Errorf("peers: %s: %w; set device_id = %q by hand", path, err, id)
	}
	pin := fmt.Sprintf("device_id = %q", id)
	replaced := false
	for i := start + 1; i < end; i++ {
		if key, _ := keyOf(lines[i]); key == "device_id" {
			lines[i] = indentOf(lines[i]) + pin + eol(lines[i])
			replaced = true
		}
	}
	if !replaced {
		l := lines[labelLine]
		nl := eol(l)
		if nl == "" { // the label is the file's last line, with no newline
			lines[labelLine] = l + "\n"
			nl = "\n"
		}
		lines = append(lines[:labelLine+1], append([]string{indentOf(l) + pin + nl}, lines[labelLine+1:]...)...)
	}
	out := []byte(strings.Join(lines, ""))
	// Check the result before it replaces the file: the entry must read back with the pin.
	specs, _, err := parse(path, out)
	if err != nil {
		return fmt.Errorf("peers: the edited file would not parse: %w; set device_id = %q by hand", err, id)
	}
	ok := false
	for _, sp := range specs {
		if sp.Label == label && sp.DeviceID == id {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("peers: %s: could not set the pin of %q; set device_id = %q by hand", path, label, id)
	}
	return replaceFile(path, out)
}

// findEntry finds the [[peers]] entry whose label line names label: the lines from its header
// up to the next table header, and its label line.
func findEntry(lines []string, label string) (start, end, labelLine int, err error) {
	type block struct{ start, end int }
	var blocks []block
	open := -1
	for i, l := range lines {
		t := strings.TrimSpace(stripComment(l))
		if !strings.HasPrefix(t, "[") {
			continue
		}
		if open >= 0 {
			blocks = append(blocks, block{open, i})
			open = -1
		}
		if strings.HasPrefix(t, "[[") && strings.HasSuffix(t, "]]") && strings.TrimSpace(t[2:len(t)-2]) == table {
			open = i
		}
	}
	if open >= 0 {
		blocks = append(blocks, block{open, len(lines)})
	}
	found := 0
	for _, bl := range blocks {
		for i := bl.start + 1; i < bl.end; i++ {
			key, val := keyOf(lines[i])
			if key != "label" {
				continue
			}
			var v struct {
				Label string `toml:"label"`
			}
			if _, err := toml.Decode("label = "+val, &v); err != nil || v.Label != label {
				continue
			}
			found++
			start, end, labelLine = bl.start, bl.end, i
		}
	}
	switch {
	case found == 0:
		return 0, 0, 0, fmt.Errorf("no [[peers]] entry with label = %q on a line of its own", label)
	case found > 1:
		return 0, 0, 0, fmt.Errorf("label %q is given %d times", label, found)
	}
	return start, end, labelLine, nil
}

// keyOf splits a "key = value" line; key is empty for any other line.
func keyOf(line string) (key, val string) {
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return "", ""
	}
	k = strings.TrimSpace(k)
	if strings.HasPrefix(k, "#") || strings.ContainsAny(k, " \t\"'[") {
		return "", ""
	}
	return k, strings.TrimSpace(v)
}

// stripComment drops a comment that starts the line, or follows the text outside quotes.
func stripComment(line string) string {
	inStr := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inStr != 0 && c == '\\' && inStr == '"':
			i++
		case inStr != 0 && c == inStr:
			inStr = 0
		case inStr == 0 && (c == '"' || c == '\''):
			inStr = c
		case inStr == 0 && c == '#':
			return line[:i]
		}
	}
	return line
}

func indentOf(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func eol(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return "\n"
	}
	return ""
}

// replaceFile writes b to a temporary file beside path and renames it over path, keeping
// path's mode. A symlink is followed: its target is replaced.
func replaceFile(path string, b []byte) error {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("peers: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".peers-*.toml")
	if err != nil {
		return fmt.Errorf("peers: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Chmod(fi.Mode().Perm())
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
		return fmt.Errorf("peers: %w", werr)
	}
	return nil
}
