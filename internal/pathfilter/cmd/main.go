// Command pathfilter prints each path read from stdin, one per line, that a GitHub Actions
// `paths` filter would trigger on. The patterns come from -patterns, a file with one pattern per
// line in the workflow's order. It prints nothing when no path triggers the filter.
//
//	yq '.on.push.paths[]' .github/workflows/ci.yml >patterns
//	git diff --name-only C T | go run ./internal/pathfilter/cmd -patterns patterns
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/noahkiss/flightdeck/internal/pathfilter"
)

func main() {
	file := flag.String("patterns", "", "file with one pattern per line (required)")
	flag.Parse()
	if *file == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(64)
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pathfilter:", err)
		os.Exit(1)
	}
	var pats []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			pats = append(pats, l)
		}
	}
	if len(pats) == 0 {
		fmt.Fprintln(os.Stderr, "pathfilter: no patterns in", *file)
		os.Exit(1)
	}
	f, err := pathfilter.Compile(pats)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pathfilter:", err)
		os.Exit(1)
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if p := sc.Text(); p != "" && f.Match(p) {
			fmt.Println(p)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "pathfilter:", err)
		os.Exit(1)
	}
}
