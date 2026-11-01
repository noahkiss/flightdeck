package link

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/cmdmarks"
	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/vt"
)

func u64(n uint64) *uint64 { return &n }

// TestPlaceLostRange: a replay of a command's range that the ring lost completely comes back at
// the ring's oldest byte, past the command's end. The run is then the empty one at the end, with
// gap, never outside the command. A range lost in part starts later, with gap.
func TestPlaceLostRange(t *testing.T) {
	done := cmdmarks.Command{ID: 90, Output: 100, End: u64(200), State: cmdmarks.Done}
	running := cmdmarks.Command{ID: 90, Output: 100, State: cmdmarks.Running}
	for _, tc := range []struct {
		name        string
		c           cmdmarks.Command
		lo, hi      uint64
		end         hp.ReplayEnd
		data        string
		start, stop uint64
		gap, more   bool
	}{
		{"all lost, done", done, 100, 200, hp.ReplayEnd{Start: 500, End: 500, Gap: true}, "", 200, 200, true, false},
		{"all lost, running", running, 100, 200, hp.ReplayEnd{Start: 500, End: 500, Gap: true}, "", 200, 200, true, true},
		{"head lost", done, 100, 200, hp.ReplayEnd{Start: 195, End: 200, Gap: true}, "tail!", 195, 200, true, false},
		{"limit lost, rest kept", done, 100, 200, hp.ReplayEnd{Start: 150, End: 150, Gap: true}, "", 150, 150, true, true},
		{"whole", done, 100, 200, hp.ReplayEnd{Start: 100, End: 200}, strings.Repeat("x", 100), 100, 200, false, false},
	} {
		got, ok := place(CommandBytes{Command: tc.c}, tc.lo, tc.hi, []byte(tc.data), &tc.end)
		if !ok || string(got.Data) != tc.data || got.Start != tc.start || got.End != tc.stop || got.Gap != tc.gap || got.More != tc.more {
			t.Errorf("%s: ok %v [%d, %d) %q gap %v more %v; want [%d, %d) %q gap %v more %v", tc.name, ok, got.Start, got.End,
				got.Data, got.Gap, got.More, tc.start, tc.stop, tc.data, tc.gap, tc.more)
		}
	}
	if _, ok := place(CommandBytes{Command: done}, 100, 200, []byte("ab"), &hp.ReplayEnd{Start: 100, End: 110}); ok {
		t.Error("an answer with bytes missing in the middle was placed")
	}
}

// TestCheckpointHeaderBound: the largest command list a tracker keeps fits the header bound,
// and a longer header is not read.
func TestCheckpointHeaderBound(t *testing.T) {
	dir := t.TempDir()
	u := uuidN(70)
	worst := make([]cmdmarks.Command, cmdmarks.MaxCommands)
	for i := range worst {
		n := uint64(1)<<63 + uint64(i)
		exit := -1 << 31
		worst[i] = cmdmarks.Command{ID: n, Prompt: u64(n), Input: u64(n), Output: n, End: u64(n), State: cmdmarks.Unknown,
			Exit: &exit, Text: strings.Repeat("\x01", cmdmarks.MaxText), TextTruncated: true,
			Started: time.Now(), Ended: time.Now()}
	}
	saved := cmdmarks.Restore(0, cmdmarks.Saved{Commands: worst}).Save()
	ck := &checkpoint{Version: checkpointVersion, Pane: u.String(), Holder: "h", Encoding: vt.EncodingVT,
		Cols: 80, Rows: 24, Snapshot: []byte("screen"), Commands: &saved}
	if err := writeCheckpoint(dir, u, ck); err != nil {
		t.Fatal(err)
	}
	got, err := readCheckpoint(dir, u)
	if err != nil {
		t.Fatalf("the largest command list: %v", err)
	}
	if len(got.Commands.Commands) != cmdmarks.MaxCommands || string(got.Snapshot) != "screen" {
		t.Fatalf("read back %d commands, snapshot %q", len(got.Commands.Commands), got.Snapshot)
	}

	// The same checkpoint with its header padded past the bound: valid JSON, but not read.
	b, err := os.ReadFile(checkpointPath(dir, u))
	if err != nil {
		t.Fatal(err)
	}
	hdr, snap, _ := bytes.Cut(b, []byte{'\n'})
	pad := append(append(append(hdr, bytes.Repeat([]byte{' '}, maxCheckpointHeader)...), '\n'), snap...)
	if err := os.WriteFile(checkpointPath(dir, u), pad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheckpoint(dir, u); err == nil || !strings.Contains(err.Error(), "header is over") {
		t.Fatalf("a header over the bound: %v", err)
	}
}

// TestRestoreTrimsLostCommands: a command in a restored checkpoint whose output the ring has
// lost is dropped when the pane is set up, though no byte is replayed after the checkpoint.
func TestRestoreTrimsLostCommands(t *testing.T) {
	const ring = 64 << 10
	hdir, state := shortDir(t), shortDir(t)
	startHolder(t, hdir, "trim", ring)
	u := uuidN(71)
	c := holderclient.New(holderclient.Config{Dir: hdir, Build: "test"})
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := c.Spawn(ctx, hp.Spawn{UUID: u, Cols: 80, Rows: 24, Cwd: "/", Env: testEnv,
		Argv: []string{"/bin/sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x; echo; exec sleep 1000"}})
	if err != nil {
		t.Fatal(err)
	}
	const count = uint64(100000 + len("\r\n"))
	waitFor(t, "the pane's output", func() bool {
		_, end, err := p.Holder.ReplayRange(ctx, p.Handle, count, count+1)
		return err == nil && end.End == count
	})
	c.Close()

	// A checkpoint at the pane's last byte, holding one command whose output left the ring and
	// one whose output is still in it.
	term, err := testParser().New(vt.Config{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	enc := testParser().Encodings()[0]
	snap, err := term.Snapshot(enc, vt.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lost := cmdmarks.Command{ID: 10, Output: 18, End: u64(20), State: cmdmarks.Done}
	kept := cmdmarks.Command{ID: count - 100, Output: count - 90, End: u64(count - 10), State: cmdmarks.Done}
	if err := writeCheckpoint(state, u, &checkpoint{Version: checkpointVersion, Pane: u.String(), Holder: "trim",
		Offset: count, Encoding: enc, Cols: 80, Rows: 24, Snapshot: snap,
		Commands: &cmdmarks.Saved{Commands: []cmdmarks.Command{lost, kept}}}); err != nil {
		t.Fatal(err)
	}
	l := openLink(t, Config{HolderDir: hdir, StateDir: state})
	if err := l.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := paneStatus(l, u); !st.Restored {
		t.Fatal("the pane did not restore the checkpoint")
	}
	cs, _, err := l.Commands(u, nil, 10)
	if err != nil || len(cs) != 1 || cs[0].ID != kept.ID {
		t.Fatalf("commands after the restore: %+v, %v; want only %d", cs, err, kept.ID)
	}
}
