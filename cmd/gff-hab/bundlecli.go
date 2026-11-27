package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/diagbundle"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/peers"
	"github.com/noahkiss/flightdeck/internal/service"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// bundleCmd writes the diagnostics bundle (diagnostics.md 8): one .tar.gz, or with -dir a
// folder, of the hab's logs, crash files, health records, tallies, config keys, friction notes,
// status, job state and detection, each file redacted in two passes. It works while gff-hab is
// down; status, detection and the names gff-hab knows then come from files only.
func bundleCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	sock := socketFlag(fs)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	holders := fs.String("holders", "", "holder registry directory (default: $GFF_HOLDER_DIR, else the holder's default)")
	config := fs.String("config", "", "the hab's config file (default: $GFF_HAB_CONFIG, else gff/hab.toml in the user config folder)")
	outFile := fs.String("o", "", "the archive to write (default: flightdeck-diagnostics-<yyyymmdd-hhmm>.tar.gz in the current folder)")
	dir := fs.String("dir", "", "write the files unpacked into this new or empty folder, for review, and make no archive")
	since := fs.String("since", "48h", "the window: logs and records from this long ago (48h) or this time (RFC 3339)")
	noFriction := fs.Bool("no-friction", false, "leave the friction notes out")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New("bundle takes no arguments")
	}
	if *dir != "" && *outFile != "" {
		return errors.New("bundle: give -o or -dir, not both")
	}
	st, err := stateDir(*state)
	if err != nil {
		return err
	}
	if st, err = filepath.Abs(st); err != nil {
		return err
	}
	now := time.Now()
	from, err := parseWhen(*since, now)
	if err != nil {
		return fmt.Errorf("bundle: -since: %v", err)
	}
	habCfg, _ := habConfigPath(*config)
	peersFile, _ := peers.DefaultPath()
	hd := holderDirOf(*holders)

	o := diagbundle.Options{State: st, Since: from, Until: now, NoFriction: *noFriction, HabConfig: habCfg, Peers: peersFile}
	o.Extras = append(o.Extras, diagbundle.Extra{Path: "versions.json", Data: versionsJSON(), Kind: diagbundle.KindKept})

	statusArgs := []string{"-state", st, "-holders", hd}
	if *sock != "" {
		statusArgs = append(statusArgs, "-socket", *sock)
	}
	var status bytes.Buffer
	if err := statusCmd(statusArgs, &status); err != nil {
		o.LeftOut = append(o.LeftOut, diagbundle.LeftOut{Item: "status.txt", Why: "gff-hab status failed"})
	} else {
		o.Extras = append(o.Extras, diagbundle.Extra{Path: "status.txt", Data: status.Bytes(), Kind: diagbundle.KindText})
	}

	var instances []string
	if es, err := holderreg.List(hd); err == nil {
		for _, e := range es {
			instances = append(instances, e.Instance)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if jobs, err := service.JobStates(ctx, st, instances, service.Exec); err != nil {
		o.LeftOut = append(o.LeftOut, diagbundle.LeftOut{Item: "service.txt", Why: "the install record does not read"})
	} else {
		var b strings.Builder
		if jobs == nil {
			b.WriteString("no install record: gff-hab is not installed as a service for this state folder\n")
		}
		for _, j := range jobs {
			b.WriteString(j.String() + "\n")
		}
		o.Extras = append(o.Extras, diagbundle.Extra{Path: "service.txt", Data: []byte(b.String()), Kind: diagbundle.KindText})
	}

	hab, detectJSON, why := bundleFromHab(ctx, *sock)
	if why != "" {
		o.LeftOut = append(o.LeftOut, diagbundle.LeftOut{Item: "detect.json", Why: why})
	} else {
		o.Extras = append(o.Extras, diagbundle.Extra{Path: "detect.json", Data: detectJSON, Kind: diagbundle.KindJSON})
	}

	home, _ := os.UserHomeDir()
	r := diagbundle.New(diagbundle.Identity{Home: home, State: st, Names: bundleNames(habCfg, peersFile, st, hab)})

	stamp := now.Format("20060102-1504")
	top := "flightdeck-diagnostics-" + stamp
	target := *dir
	var archive string
	if target == "" {
		archive = *outFile
		if archive == "" {
			archive = top + ".tar.gz"
		}
		if archive, err = filepath.Abs(archive); err != nil {
			return err
		}
		if target, err = os.MkdirTemp(filepath.Dir(archive), ".flightdeck-diagnostics-"); err != nil {
			return err
		}
		defer os.RemoveAll(target)
	} else if err := emptyDir(target); err != nil {
		return err
	}
	m, err := diagbundle.Write(target, r, o)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	where := target
	var size int64
	for _, f := range m.Files {
		size += f.Size
	}
	if archive != "" {
		if err := diagbundle.Archive(archive, target, top); err != nil {
			return fmt.Errorf("bundle: %w", err)
		}
		where = archive
		if fi, err := os.Stat(archive); err == nil {
			size = fi.Size()
		}
	}
	fmt.Fprintf(out, "%s (%s, %d files)\n", where, fmtSize(size), len(m.Files)+1)
	var counts []string
	for _, rule := range diagbundle.Rules {
		if n := m.Replacements[rule]; n > 0 {
			counts = append(counts, fmt.Sprintf("%s %d", rule, n))
		}
	}
	if len(counts) == 0 {
		counts = []string{"none"}
	}
	fmt.Fprintf(out, "replacements: %s\n", strings.Join(counts, ", "))
	for _, l := range m.LeftOut {
		fmt.Fprintf(out, "left out: %s: %s\n", l.Item, l.Why)
	}
	fmt.Fprintln(out, "Review the files before you share them.")
	return nil
}

// emptyDir makes dir, or checks that it is an empty folder.
func emptyDir(dir string) error {
	es, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.MkdirAll(dir, 0o700)
	case err != nil:
		return err
	case len(es) > 0:
		return fmt.Errorf("bundle: %s is not empty", dir)
	}
	return nil
}

// versionsJSON is versions.json: gff-hab's version and build, Go's version, the system and its
// version, and the CPU.
func versionsJSON() []byte {
	osVersion := ""
	if runtime.GOOS == "darwin" {
		if b, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			osVersion = strings.TrimSpace(string(b))
		}
	} else if b, err := exec.Command("uname", "-r").Output(); err == nil {
		osVersion = strings.TrimSpace(string(b))
	}
	b, _ := json.MarshalIndent(map[string]string{"gff_hab": version, "build": build, "go": runtime.Version(),
		"os": runtime.GOOS, "os_version": osVersion, "arch": runtime.GOARCH}, "", "  ")
	return append(b, '\n')
}

// bundleDetect is one pane's entry in detect.json: detect explain --json --no-text, or why it
// could not be read.
type bundleDetect struct {
	Pane    string             `json:"pane"`
	Explain *api.DetectExplain `json:"explain,omitempty"`
	Error   string             `json:"error,omitempty"`
}

// bundleFromHab asks a running gff-hab for its structure and for detect.explain on each of its
// live panes, with no text (8.2). why says why there is no detect.json.
func bundleFromHab(ctx context.Context, sock string) (*api.Structure, []byte, string) {
	c, err := open(sock, "")
	if err != nil {
		return nil, nil, "gff-hab does not answer"
	}
	defer c.Close()
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.Hello(hctx, cliClient, "", ""); err != nil {
		return nil, nil, "gff-hab does not answer"
	}
	cl := api.Client{Caller: c}
	sr, err := cl.StructureGet(hctx, api.StructureGetParams{})
	if err != nil {
		return nil, nil, "structure.get failed"
	}
	out := []bundleDetect{}
	for _, p := range sr.Panes {
		d := bundleDetect{Pane: p.ID}
		pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
		res, err := cl.DetectExplain(pctx, api.DetectExplainParams{Pane: p.ID})
		pcancel()
		if err != nil {
			d.Error = errorKindWord(err)
		} else {
			noText(&res)
			d.Explain = &res
		}
		out = append(out, d)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return &sr.Structure, nil, "detect.json does not encode"
	}
	return &sr.Structure, b, ""
}

// errorKindWord is an error's API kind, never its text.
func errorKindWord(err error) string {
	if k := errKind(err); k != "" {
		return string(k)
	}
	return "failed"
}

// bundleNames are the names pass 2 replaces wherever they stand (8.3): this machine's host and
// user names; each host, label and device id of the config files; the projects gff-hab knows,
// from its structure file, the project roots, and a running gff-hab.
func bundleNames(habCfg, peersFile, state string, st *api.Structure) []string {
	var names []string
	add := func(vs ...string) {
		for _, v := range vs {
			if v = strings.TrimSpace(v); v != "" && !slices.Contains(names, v) {
				names = append(names, v)
			}
		}
	}
	if h, err := os.Hostname(); err == nil {
		short, _, _ := strings.Cut(h, ".")
		add(h, short)
	}
	if u, err := user.Current(); err == nil {
		add(u.Username)
	}
	add(os.Getenv("USER"), os.Getenv("LOGNAME"))
	for _, p := range []string{habCfg, peersFile} {
		var tree map[string]any
		if p == "" {
			continue
		}
		if _, err := toml.DecodeFile(p, &tree); err == nil {
			configNames(tree, add)
		}
	}
	if b, err := os.ReadFile(filepath.Join(state, structure.StateFile)); err == nil {
		var f struct {
			Projects []struct {
				Name string   `json:"name"`
				Dirs []string `json:"dirs"`
			} `json:"projects"`
		}
		if json.Unmarshal(b, &f) == nil {
			for _, p := range f.Projects {
				add(p.Name)
				for _, d := range p.Dirs {
					add(filepath.Base(d))
				}
			}
		}
	}
	if st != nil {
		add(st.Host.ID)
		for _, p := range st.Projects {
			add(p.Name)
		}
	}
	return names
}

// configNames walks a config file's tree for names: the values under the keys that hold a
// host, a label, a device id or a name, the host part of each endpoint and URL, and each
// project root's folder and the projects in it.
func configNames(v any, add func(...string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			switch k {
			case "label", "host", "host_id", "hostname", "device", "device_id", "name":
				if s, ok := e.(string); ok {
					add(s)
				}
			case "endpoints", "url", "urls", "endpoint":
				for _, s := range stringsOf(e) {
					add(endpointHost(s))
				}
			case "project_roots":
				for _, root := range stringsOf(e) {
					add(filepath.Base(root))
					if es, err := os.ReadDir(root); err == nil {
						for _, d := range es {
							if d.IsDir() {
								add(d.Name())
							}
						}
					}
				}
			}
			configNames(e, add)
		}
	case []any:
		for _, e := range x {
			configNames(e, add)
		}
	case []map[string]any:
		for _, e := range x {
			configNames(e, add)
		}
	}
}

func stringsOf(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// endpointHost is the host part of an endpoint or a URL: no scheme, user, port or path.
func endpointHost(s string) string {
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "[") {
		s, _, _ = strings.Cut(strings.TrimPrefix(s, "["), "]")
		return s
	}
	if h, port, ok := strings.Cut(s, ":"); ok && !strings.Contains(port, ":") {
		return h
	}
	return s
}
