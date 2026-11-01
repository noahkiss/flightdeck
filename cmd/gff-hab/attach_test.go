package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/attach"
)

// fakeIdleClaude reports idle with OSC 7501, then reads one message of the length the test
// writes to msglen.
const fakeIdleClaude = `
stty raw -echo
printf '\033]7501;state=idle:app=claude\033\\'
while [ ! -e "$DIR/msglen" ]; do sleep 0.02; done
head -c "$(cat "$DIR/msglen")" > "$DIR/msg.tmp" && mv "$DIR/msg.tmp" "$DIR/msg"
while [ ! -e "$DIR/end" ]; do sleep 0.02; done
`

// TestAttachmentEndToEnd: with the real holder and gff-hab, the welcome lists attachments; a
// PNG uploaded in two runs lands in the session's folder under .flightdeck/attachments, which
// ignores itself; attachment.get returns a thumbnail; session.send with the id delivers the text
// and the path as one paste plus Enter; a send with an id of another session is not_found.
func TestAttachmentEndToEnd(t *testing.T) {
	e := newHabEnv(t, 0)
	if w := welcome(t, e.sock); !slices.Contains(w.Capabilities, "attachments") {
		t.Fatalf("capabilities %v lack attachments", w.Capabilities)
	}
	c := e.control()
	cl := api.Client{Caller: c}
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	proj := filepath.Join(e.work, "proj")
	if err := os.Mkdir(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	var ss api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{
		Name: "claude", Argv: []string{"/bin/sh", "-c", fakeIdleClaude}, Cwd: proj, Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, noRaceExitSleep}}, &ss); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.WriteFile(filepath.Join(e.work, "end"), nil, 0o600) })

	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	h := sha256.Sum256(b)
	st, err := cl.AttachmentPut(ctx, api.AttachmentPutParams{Session: ss.Session, Name: "bug shot.png", Type: "image/png", Size: uint64(len(b)), SHA256: hex.EncodeToString(h[:])})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(proj, attach.Folder) + "/"; !strings.HasPrefix(evalDir(t, st.Path), evalDir(t, filepath.Join(proj, attach.Folder))+"/") {
		t.Fatalf("path %s is not under %s", st.Path, want)
	}
	half := len(b) / 2
	if w, err := cl.AttachmentWrite(ctx, api.AttachmentWriteParams{ID: st.ID, Data: b[:half]}); err != nil || w.Done {
		t.Fatalf("first run: %+v %v", w, err)
	}
	if w, err := cl.AttachmentWrite(ctx, api.AttachmentWriteParams{ID: st.ID, Offset: int64(half), Data: b[half:]}); err != nil || !w.Done || w.Received != int64(len(b)) {
		t.Fatalf("last run: %+v %v", w, err)
	}
	if got, err := os.ReadFile(st.Path); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("stored file: %v", err)
	}
	if ign, err := os.ReadFile(filepath.Join(proj, ".flightdeck", ".gitignore")); err != nil || !strings.Contains(string(ign), "\n*\n") {
		t.Errorf("ignore file %q, %v", ign, err)
	}
	th, err := cl.AttachmentGet(ctx, api.AttachmentGetParams{ID: st.ID, Thumbnail: true})
	if err != nil || th.Type != "image/jpeg" || len(th.Data) == 0 {
		t.Fatalf("thumbnail: %s %d bytes %v", th.Type, len(th.Data), err)
	}

	text := "this is the bug"
	want := "\x1b[200~" + text + "\n\n" + st.Path + "\x1b[201~\r"
	waitState(t, e.sock, ss.Pane, "OSC idle", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle })
	writeFile(t, filepath.Join(e.work, "msglen"), strconv.Itoa(len(want)))
	if err := c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: text, Attachments: []string{st.ID}, Origin: api.SendOriginTyped}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitUntil(t, "the message with its path in the pane", fileIs(filepath.Join(e.work, "msg"), want))
	err = c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: "x", Attachments: []string{"0123456789ab"}}, nil)
	if errKind(err) != api.ErrorKindNotFound {
		t.Errorf("send with an unknown id: %v", err)
	}
}

func evalDir(t *testing.T, p string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, filepath.Base(p))
}

// TestChatAttachments: a user message whose trailing lines are paths of stored attachments
// shows its text and their ids; other messages, and paths of files the hab did not store, stay
// as they are; the reader's own slice is never changed.
func TestChatAttachments(t *testing.T) {
	p := "/w/proj/.flightdeck/attachments/0123456789ab-shot.png"
	c := &chatService{chatConfig: chatConfig{attachment: func(id string) bool { return id == "0123456789ab" }}}
	in := []api.ChatMessage{
		{ID: "1", Kind: api.ChatKindUser, Text: "look\n\n" + p},
		{ID: "2", Kind: api.ChatKindAgent, Text: "look\n\n" + p},
		{ID: "3", Kind: api.ChatKindUser, Text: "/w/proj/.flightdeck/attachments/ba9876543210-x.png"},
	}
	out := c.withAttachments(in)
	if out[0].Text != "look" || !slices.Equal(out[0].Attachments, []string{"0123456789ab"}) {
		t.Errorf("user message: %+v", out[0])
	}
	if out[1].Text != in[1].Text || out[1].Attachments != nil || out[2].Text != in[2].Text || out[2].Attachments != nil {
		t.Errorf("other messages changed: %+v %+v", out[1], out[2])
	}
	if in[0].Text != "look\n\n"+p || in[0].Attachments != nil {
		t.Errorf("the reader's slice changed: %+v", in[0])
	}

	// A message whose harness turned the paths into images matches its send.
	c.matchSend = func(session, text string, at time.Time) (string, []string, bool) {
		if session == "s1" && attach.CleanText(text) == "see this" {
			return "see this", []string{"0123456789ab", "ba9876543210"}, true
		}
		return text, nil, false
	}
	got := c.withAttachments([]api.ChatMessage{
		{ID: "4", Session: "s1", Kind: api.ChatKindUser, At: "2026-03-01T12:00:00Z", Text: "see this\n\n[Image #1]\n\n[image]"},
		{ID: "5", Session: "s1", Kind: api.ChatKindUser, At: "2026-03-01T12:00:00Z", Text: "no match [image]"},
	})
	if got[0].Text != "see this" || !slices.Equal(got[0].Attachments, []string{"0123456789ab", "ba9876543210"}) {
		t.Errorf("matched message: %+v", got[0])
	}
	if got[1].Text != "no match [image]" || got[1].Attachments != nil {
		t.Errorf("unmatched message changed: %+v", got[1])
	}
}
