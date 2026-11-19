package tallies

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

// PrintWindow writes the figures of a window as text: calls and errors per method and client
// kind, events, hook events, features, settings and health (7.4).
func PrintWindow(out io.Writer, w Window) error {
	d := &w.Sum
	clients := map[string]bool{}
	for _, cs := range d.Calls {
		for c := range cs {
			clients[c] = true
		}
	}
	fmt.Fprintf(out, "window: %s to %s (%d day files)\n", w.From, w.To, w.Files)
	fmt.Fprintf(out, "client kinds seen: %s\n", orNone(sortedKeys(clients)))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if len(d.Calls) > 0 {
		fmt.Fprintln(tw, "\nMETHOD\tCLIENT\tCALLS\tERRORS")
		for _, m := range sortedKeys(d.Calls) {
			for _, c := range sortedKeys(d.Calls[m]) {
				x := d.Calls[m][c]
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", m, c, x.N, errText(x.Errors))
			}
		}
	}
	counts := func(title string, m map[string]uint64) {
		if len(m) == 0 {
			return
		}
		fmt.Fprintf(tw, "\n%s\tCOUNT\n", title)
		for _, k := range sortedKeys(m) {
			fmt.Fprintf(tw, "%s\t%d\n", k, m[k])
		}
	}
	counts("EVENT", d.Events)
	if len(d.Hooks) > 0 {
		fmt.Fprintln(tw, "\nHOOK\tCOUNT\tREFUSED")
		for _, k := range sortedKeys(d.Hooks) {
			fmt.Fprintf(tw, "%s\t%d\t%d\n", k, d.Hooks[k].N, d.Hooks[k].Refused)
		}
	}
	counts("FEATURE", d.Features)
	if len(d.Settings) > 0 {
		fmt.Fprintln(tw, "\nSETTING\tSET\tIN CONFIG")
		for _, k := range sortedKeys(d.Settings) {
			s := d.Settings[k]
			fmt.Fprintf(tw, "%s\t%d\t%v\n", k, s.Set, s.InConfig)
		}
	}
	if len(d.Health) > 0 {
		fmt.Fprintln(tw, "\nPROCESS\tSTARTS\tCRASHES\tUNCLEAN")
		for _, k := range sortedKeys(d.Health) {
			h := d.Health[k]
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", k, h.Starts, h.Crashes, h.Unclean)
		}
	}
	return tw.Flush()
}

// PrintUnused writes the unused names of the surface, by kind (7.4).
func PrintUnused(out io.Writer, us []Unused, since string, days, seenDays int) error {
	if since == "" {
		since = "tallying started"
	}
	fmt.Fprintf(out, "unused in the last %d days (tallies since %s, %d day files in the window): %d names\n",
		days, since, seenDays, len(us))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAME\tLAST USED")
	for _, u := range us {
		last := u.Last
		if last == "" {
			last = "never since " + since
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Kind, u.Name, last)
	}
	return tw.Flush()
}

func errText(m map[string]uint64) string {
	if len(m) == 0 {
		return "0"
	}
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func orNone(s []string) string {
	s = slices.DeleteFunc(s, func(x string) bool { return x == "" })
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}
