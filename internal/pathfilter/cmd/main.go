// Command pathfilter prints each path read from stdin, one per line, that a GitHub Actions
// `paths` filter would trigger on. The patterns come from -patterns, a file with one pattern per
// line in the workflow's order. It prints nothing when no path triggers the filter.
//
//	yq '.on.push.paths[]' .github/workflows/ci.yml >patterns
//	git -c core.quotePath=false diff --name-only C T | go run ./internal/pathfilter/cmd -patterns patterns
//
// Exit status: 0 on success, 1 when the pattern file is missing, empty or holds a bad pattern,
// 64 on a usage error.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/noahkiss/flightdeck/internal/pathfilter"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pathfilter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("patterns", "", "file with one pattern per line (required)")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *file == "" || fs.NArg() != 0 {
		fs.Usage()
		return 64
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(stderr, "pathfilter:", err)
		return 1
	}
	var pats []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			pats = append(pats, l)
		}
	}
	if len(pats) == 0 {
		fmt.Fprintln(stderr, "pathfilter: no patterns in", *file)
		return 1
	}
	f, err := pathfilter.Compile(pats)
	if err != nil {
		fmt.Fprintln(stderr, "pathfilter:", err)
		return 1
	}
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		if p := sc.Text(); p != "" && f.Match(p) {
			fmt.Fprintln(stdout, p)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(stderr, "pathfilter:", err)
		return 1
	}
	return 0
}
