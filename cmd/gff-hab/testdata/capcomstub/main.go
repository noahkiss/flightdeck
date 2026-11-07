// Command capcomstub stands in for the Capcom engine in gff-hab's tests (capcom-service.md
// 4.4). It reaches gff-hab's engine route with GFF_HAB's socket and GFF_HAB_CAPCOM_TOKEN, and the
// pane route with GFF_HAB's token; it takes hello, answers ready, and runs one turn per
// input: turn.started, input.taken, a reply "echo: <text>", turn.ended. It reconnects after a
// drop and sends again what gff-hab did not acknowledge. Its log, $STUB_DIR/log, records what
// the tests check: its pid, a hash of the credential, whether its environment holds one.
//
// Inputs that steer it: "slow" waits for $STUB_DIR/release before its reply; "crash" exits 3;
// "route" calls channel.status on the pane route with its token and replies with the answer;
// "lbwrite" files a Logbook task on the pane route and replies with the answer;
// "approve" asks approval.request for a WebFetch and replies with the answer it got. With the
// file $STUB_DIR/crashstart present it exits 3 at once, before it connects.
//
// The context manager (capcom-service.md 6): "remember <title>" files a capcom guide on the pane
// route, "guide <title>" a guide with no tag, "everywhere <title>" one tagged capcom and everywhere, "edit <id> <title>" retitles a record; "ctx <n>"
// ends its turn with context_used n; "compact" asks context.request for a compaction, sends a
// summary and compacted. A "reseed" asks context.request for a re-seed, writes the context it
// got to $STUB_DIR/context-<n>.json, sends a summary (none for new) and session.started. A text
// that starts with "event:" runs as an event turn.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	dir      = os.Getenv("STUB_DIR")
	logMu    sync.Mutex
	outMu    sync.Mutex
	outbox   []map[string]any // sent, not acknowledged
	seq      uint64
	cur      net.Conn
	curW     *bufio.Writer
	turnN    int
	inputs   = make(chan map[string]any, 100)
	answer   = make(chan map[string]any, 10)
	contexts = make(chan map[string]any, 10)
	reseeds  = make(chan string, 10)
	ctxN     int
)

func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, format+"\n", args...)
	f.Close()
}

func main() {
	addr := os.Getenv("GFF_HAB")
	rest := strings.TrimPrefix(addr, "http+unix://")
	i := strings.LastIndex(rest, "/hooks/")
	sock, paneToken := rest[:i], rest[i+len("/hooks/"):]
	token := os.Getenv("GFF_HAB_CAPCOM_TOKEN")
	logf("pid %d", os.Getpid())
	if _, err := os.Stat(filepath.Join(dir, "crashstart")); err == nil {
		logf("crash at start")
		os.Exit(3)
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=") || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			logf("env holds a credential")
		}
	}
	go turns(sock, paneToken)
	for {
		if err := session(sock, token); err != nil {
			logf("connection: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// session runs one connection: hello, ready, the resend, then messages until it drops.
func session(sock, token string) error {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /capcom/%s HTTP/1.1\r\nHost: hab\r\nConnection: Upgrade\r\nUpgrade: gff-capcom/1\r\nContent-Length: 0\r\n\r\n", token)
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("upgrade: %d", resp.StatusCode)
	}
	outMu.Lock()
	cur, curW, seq = c, bufio.NewWriter(c), 0
	outMu.Unlock()
	readyDone := false
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			outMu.Lock()
			cur, curW = nil, nil
			outMu.Unlock()
			return err
		}
		var m map[string]any
		json.Unmarshal(line, &m)
		typ, _ := m["type"].(string)
		if typ == "ack" {
			ack := uint64(m["ack"].(float64))
			outMu.Lock()
			kept := outbox[:0]
			for _, o := range outbox {
				if s := o["seq"].(uint64); s == 0 || s > ack {
					kept = append(kept, o)
				}
			}
			outbox = kept
			outMu.Unlock()
			continue
		}
		write(map[string]any{"type": "ack", "ack": m["seq"]}, false)
		switch typ {
		case "hello":
			cred, _ := m["credential"].(map[string]any)
			v, _ := cred["value"].(string)
			sum := sha256.Sum256([]byte(v))
			logf("hello %s %s", cred["kind"], hex.EncodeToString(sum[:8]))
			if st, ok := m["settings"].(map[string]any); ok {
				logf("settings max_turns %v", st["max_turns"])
			}
			if !readyDone {
				readyDone = true
			}
			send(map[string]any{"type": "ready", "engine": "stub", "version": "1", "capabilities": []string{"interrupt", "mid_turn_input"}})
			// Resend what the last connection left unacknowledged, in order, with new numbers.
			outMu.Lock()
			old := outbox
			outbox = nil
			outMu.Unlock()
			for _, o := range old {
				delete(o, "seq")
				send(o)
			}
		case "input":
			inputs <- m["item"].(map[string]any)
		case "approval.answer":
			answer <- m
		case "context":
			if _, asked := m["request"].(string); asked {
				contexts <- m
			}
		case "reseed":
			reason, _ := m["reason"].(string)
			logf("reseed %s", reason)
			reseeds <- reason
		case "interrupt":
			logf("interrupt")
		case "shutdown":
			logf("shutdown")
			os.Exit(0)
		}
	}
}

// askContext sends context.request and waits for the answer, which it writes to a file.
func askContext(purpose string) map[string]any {
	ctxN++
	req := fmt.Sprintf("req-%d-%d", os.Getpid(), ctxN)
	send(map[string]any{"type": "context.request", "request": req, "for": purpose})
	select {
	case c := <-contexts:
		b, _ := json.MarshalIndent(c, "", "  ")
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("context-%d.json", ctxN)), b, 0o600)
		return c
	case <-time.After(10 * time.Second):
		logf("no context answer")
		return nil
	}
}

func summarise(c map[string]any) {
	if c == nil {
		return
	}
	recent, _ := c["recent"].([]any)
	through, _ := c["through"].(string)
	send(map[string]any{"type": "summary", "text": fmt.Sprintf("stub summary of %d items", len(recent)), "through": through})
}

// reseed runs a re-seed between turns: the context, the summary, a fresh session.
func reseed(reason string) {
	c := askContext("reseed")
	if reason != "new" {
		summarise(c)
	}
	why := "reseed"
	if reason == "new" {
		why = "new"
	}
	ctxN++
	send(map[string]any{"type": "session.started", "session": fmt.Sprintf("s-%d-%d", os.Getpid(), ctxN), "why": why})
}

// turns runs one turn per input, in order, and a re-seed between turns.
func turns(sock, token string) {
	for {
		var it map[string]any
		select {
		case reason := <-reseeds:
			reseed(reason)
			continue
		case it = <-inputs:
		}
		id, _ := it["id"].(string)
		text, _ := it["text"].(string)
		turnN++
		turn := fmt.Sprintf("t%d-%d", os.Getpid(), turnN)
		logf("input %s %s", id, text)
		origin := "user"
		if t, ok := strings.CutPrefix(text, "event:"); ok {
			origin, text = "event", t
		}
		send(map[string]any{"type": "turn.started", "turn": turn, "origin": origin, "items": []string{id}})
		send(map[string]any{"type": "input.taken", "item": id})
		reply := "echo: " + text
		used := 0.0
		verb, arg, _ := strings.Cut(text, " ")
		switch verb {
		case "remember", "guide", "everywhere":
			tags := `,"tags":["capcom"]`
			switch verb {
			case "guide":
				tags = ""
			case "everywhere":
				tags = `,"tags":["capcom","everywhere"]`
			}
			body := fmt.Sprintf(`{"method":"logbook.write","params":{"idempotency_key":"stub-%s","changes":[{"op":"create","type":"guide","title":%q%s}]}}`, id, arg, tags)
			reply = "lb: " + routeCall(sock, token, body)
		case "edit":
			rid, title, _ := strings.Cut(arg, " ")
			body := fmt.Sprintf(`{"method":"logbook.write","params":{"idempotency_key":"stub-%s","changes":[{"op":"set","id":%q,"field":"title","value":%q}]}}`, id, rid, title)
			reply = "lb: " + routeCall(sock, token, body)
		case "ctx":
			fmt.Sscanf(arg, "%g", &used)
		case "compact":
			summarise(askContext("compact"))
			send(map[string]any{"type": "compacted"})
		}
		switch text {
		case "crash":
			logf("crashing")
			os.Exit(3)
		case "slow":
			for {
				if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		case "route":
			reply = "route: " + routeCall(sock, token, `{"method":"channel.status","params":{"id":"m-none"}}`)
		case "lbwrite":
			reply = "lb: " + routeCall(sock, token, `{"method":"logbook.write","params":{"idempotency_key":"stub-lb-1","changes":[{"op":"create","type":"task","title":"from capcom"}]}}`)
		case "approve":
			req := "req-" + turn
			send(map[string]any{"type": "approval.request", "request": req, "tool": "WebFetch",
				"input": map[string]any{"url": "https://example.com/x"}, "reason": "Capcom wants to open example.com/x"})
			a := <-answer
			reply = fmt.Sprintf("approved: %v", a["allow"])
		}
		send(map[string]any{"type": "reply", "turn": turn, "part": 0, "text": reply})
		send(map[string]any{"type": "turn.ended", "turn": turn, "result": "success", "context_used": used})
	}
}

func routeCall(sock, token, body string) string {
	cl := http.Client{Transport: &http.Transport{Dial: func(string, string) (net.Conn, error) { return net.Dial("unix", sock) }}}
	resp, err := cl.Post("http://hab/channel/"+token, "application/json", bytes.NewBufferString(body))
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

// send numbers m, keeps it until acknowledged, and writes it when connected.
func send(m map[string]any) { write(m, true) }

func write(m map[string]any, keep bool) {
	outMu.Lock()
	defer outMu.Unlock()
	if keep {
		seq++
		m["seq"] = seq
		outbox = append(outbox, m)
	}
	if curW == nil {
		if keep {
			seq-- // numbered again on the next connection
			delete(m, "seq")
			m["seq"] = uint64(0)
		}
		return
	}
	b, _ := json.Marshal(m)
	curW.Write(append(b, '\n'))
	curW.Flush()
}
