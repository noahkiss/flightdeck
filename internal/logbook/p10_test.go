package logbook

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// P10. Two writers on one file: two processes, one as gff-hab (it names the origin) and one as
// the CLI fallback (it takes the origin from the file), each with two goroutines writing, never
// reuse a seq or lose a write (4.3). Under go test -race the writer processes are race builds too.
func TestP10TwoWritersOneFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	per := envInt("GFF_LOGBOOK_P10_WRITES", map[bool]int{false: 100, true: 2000}[nightly()])
	if testing.Short() {
		per = 20
	}
	path := filepath.Join(t.TempDir(), "logbook.db")
	hab := openReplica(t, "hab", path)
	if err := hab.Close(); err != nil {
		t.Fatal(err)
	}
	type proc struct {
		cmd *exec.Cmd
		out *strings.Builder
		in  *os.File
	}
	var procs []proc
	for i, origin := range []string{hab.Origin(), ""} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestP10Writer$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "GFF_LOGBOOK_P10_FILE="+path, "GFF_LOGBOOK_P10_ORIGIN="+origin,
			fmt.Sprintf("GFF_LOGBOOK_P10_N=%d", per), fmt.Sprintf("GFF_LOGBOOK_P10_NAME=w%d", i))
		var out strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &out
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdin = r
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		procs = append(procs, proc{cmd, &out, w})
	}
	// Both are up: start them together.
	for _, p := range procs {
		_, _ = p.in.WriteString("go\n")
		_ = p.in.Close()
	}
	reported := map[OpID]string{}
	for _, p := range procs {
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("writer %d: %v\n%s", p.cmd.Process.Pid, err, p.out.String())
		}
		sc := bufio.NewScanner(strings.NewReader(p.out.String()))
		for sc.Scan() {
			var op, rec string
			if _, err := fmt.Sscanf(sc.Text(), "P10 %s %s", &op, &rec); err != nil {
				continue
			}
			id, err := ParseOpID(op)
			if err != nil {
				t.Fatal(err)
			}
			if prev, dup := reported[id]; dup {
				t.Fatalf("op %v written twice: for %s and %s", id, prev, rec)
			}
			reported[id] = rec
		}
	}
	total := 2 * 2 * per * 2 // processes, goroutines, writes, ops per write
	if len(reported) != total {
		t.Fatalf("writers reported %d ops, want %d", len(reported), total)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Ops != total || st.Vector[hab.Origin()] != uint64(total) || st.Waiting != 0 || st.Records != total/2 {
		t.Fatalf("status %+v: want %d ops with seqs 1 to %d, all folded, %d records", st, total, total, total/2)
	}
	all, _, err := s.OpsAfter(ctx, Vector{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var last *Op
	for i, raw := range all {
		o, err := decodeOp(raw)
		if err != nil {
			t.Fatal(err)
		}
		if o.Seq != uint64(i+1) {
			t.Fatalf("op %d has seq %d", i+1, o.Seq)
		}
		if rec, ok := reported[o.ID()]; !ok || rec != o.Record {
			t.Fatalf("op %v on %s was not reported for that record (%q)", o.ID(), o.Record, rec)
		}
		// Stamps never go back: a later batch orders after an earlier one, whoever wrote it.
		if last != nil && last.BatchLast < o.Seq && !last.before(o) {
			t.Fatalf("op %v stamped %v, not after %v at %v", o.ID(), o.HLC(), last.ID(), last.HLC())
		}
		last = o
	}
	for _, rec := range reported {
		if r, err := s.Get(ctx, rec); err != nil || len(r.Notes) != 1 {
			t.Fatalf("record %s: %v %v", rec, r, err)
		}
	}
	if d := dump(t, s); d != func() string {
		if err := s.Rebuild(ctx); err != nil {
			t.Fatal(err)
		}
		return dump(t, s)
	}() {
		t.Fatal("the file's folded tables differ from a rebuild")
	}
}

// TestP10Writer is the writer process of TestP10TwoWritersOneFile; it does nothing on its own.
func TestP10Writer(t *testing.T) {
	path := os.Getenv("GFF_LOGBOOK_P10_FILE")
	if path == "" {
		t.Skip("run by TestP10TwoWritersOneFile")
	}
	n := envInt("GFF_LOGBOOK_P10_N", 1)
	name := os.Getenv("GFF_LOGBOOK_P10_NAME")
	s, err := Open(context.Background(), path, Options{Origin: os.Getenv("GFF_LOGBOOK_P10_ORIGIN")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var lines []string
	errs := make(chan error, 2)
	for g := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range n {
				id := NewID(TypeTask)
				res, err := s.Write(context.Background(), Batch{Steps: []Step{
					create(id, TypeTask, map[string]any{"title": fmt.Sprintf("%s g%d %d", name, g, i)}),
					note(id, "written at "+time.Now().Format(time.RFC3339Nano)),
				}})
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				for _, o := range res.Ops {
					lines = append(lines, fmt.Sprintf("P10 %s %s", o, id))
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	slices.Sort(lines)
	fmt.Println(strings.Join(lines, "\n"))
}
