package gffclient

// The heavy-run lock on the status board (groups.md 8): one line per host whose lock is in use
// or whose disk is low, from the catalog's heavy section.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// heavyCopy is one host's heavy section, from the newest catalog entry read.
type heavyCopy struct {
	host    string // the host's label in gff, else its host id
	section api.CatalogHeavy
	own     bool // from the host's own hab, not a peer's copy
	seenAt  time.Time
}

// keepHeavy keeps a host's heavy section. The answering hab's own entry always wins: it is
// live. A peer's copy is kept for a host in gff's host list when it is newer than the one held
// and no own entry was read. labels maps device ids to labels.
func (u *workspaceUI) keepHeavy(answering string, e api.CatalogEntry, labels map[string]string) {
	id := e.Catalog.DeviceID
	if id == "" {
		return
	}
	label := answering
	if !e.Own {
		var ok bool
		if label, ok = labels[id]; !ok {
			return
		}
	}
	seen, _ := time.Parse(time.RFC3339Nano, e.SeenAt)
	old, had := u.heavy[id]
	if had && !e.Own && (old.own || !seen.After(old.seenAt)) {
		return
	}
	if e.Catalog.Heavy == nil {
		delete(u.heavy, id)
		return
	}
	u.heavy[id] = heavyCopy{host: label, section: *e.Catalog.Heavy, own: e.Own, seenAt: seen}
}

// heavyLines are the board's lines of the heavy-run lock: hosts with runs holding or waiting,
// or with low disk, by host.
func (u *workspaceUI) heavyLines() []string {
	var cs []heavyCopy
	for _, c := range u.heavy {
		s := c.section
		if s.Holding+s.Waiting > 0 || s.DiskLow {
			cs = append(cs, c)
		}
	}
	slices.SortFunc(cs, func(a, b heavyCopy) int { return strings.Compare(a.host, b.host) })
	var out []string
	for _, c := range cs {
		out = append(out, heavyText(c))
	}
	return out
}

func heavyText(c heavyCopy) string {
	s := c.section
	parts := []string{fmt.Sprintf("%d running", s.Holding)}
	if s.Waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting", s.Waiting))
	}
	if s.DiskLow {
		low := "disk low"
		var least *api.HeavyVolume
		for i := range s.Volumes {
			if least == nil || s.Volumes[i].FreeBytes < least.FreeBytes {
				least = &s.Volumes[i]
			}
		}
		if least != nil {
			low += fmt.Sprintf(": %.1f GiB free", float64(least.FreeBytes)/float64(1<<30))
		}
		parts = append(parts, low+", runs wait")
	}
	return fmt.Sprintf("  %-10s %s", middle(c.host, 10), strings.Join(parts, " · "))
}
