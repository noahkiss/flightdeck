// Command fakeopencode stands in for opencode 2.2.5 in gff-hab's tests. `fakeopencode serve
// --stdio --hostname h --port p` serves the few API operations the adapter uses, with the
// password from OPENCODE_PASSWORD, and ends when its stdin closes or on SIGTERM. Any other
// argv is the TUI: it records its argv and waits for $DIR/end. Everything it does is driven by
// files in $DIR: each line the test appends to $DIR/emit is sent as an event, prompts go to
// $DIR/prompts, and permission replies to $DIR/replies.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/internal/opencode"
)

var dir = os.Getenv("DIR")

func appendLine(name, line string) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(f, line)
	f.Close()
}

func envState() string {
	pw := "absent"
	if os.Getenv(opencode.PasswordEnv) != "" {
		pw = "set"
	}
	gff := "absent"
	if _, ok := os.LookupEnv("GFF_OPENCODE_PASSWORD"); ok {
		gff = "present"
	}
	return "password=" + pw + " gff=" + gff
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve()
		return
	}
	appendLine("tui.args", strings.Join(os.Args[1:], " ")+" | "+envState())
	fmt.Println("fake opencode TUI")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM)
	for {
		select {
		case <-sigs:
			return
		case <-time.After(20 * time.Millisecond):
		}
		if _, err := os.Stat(filepath.Join(dir, "end")); err == nil {
			return
		}
	}
}

type server struct {
	mu       sync.Mutex
	subs     map[chan string]bool
	pending  map[string]string // request id: session
	messages []string          // raw messages, oldest first
	n        int
}

func (s *server) send(ev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var e struct {
		Type string `json:"type"`
		Data struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(ev), &e) == nil && e.Type == "permission.asked" {
		s.pending[e.Data.ID] = e.Data.SessionID
	}
	for c := range s.subs {
		select {
		case c <- ev:
		default:
		}
	}
}

func serve() {
	port := ""
	for i, a := range os.Args {
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	password := os.Getenv(opencode.PasswordEnv)
	appendLine("server.env", envState())
	s := &server{subs: map[chan string]bool{}, pending: map[string]string{}}
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"2.2.5"}`)
	})
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		paths := map[string]map[string]map[string]string{}
		for _, op := range opencode.Operations {
			if paths[op.Path] == nil {
				paths[op.Path] = map[string]map[string]string{}
			}
			paths[op.Path][strings.ToLower(op.Method)] = map[string]string{"operationId": op.ID}
		}
		json.NewEncoder(w).Encode(map[string]any{"paths": paths})
	})
	mux.HandleFunc("GET /api/session/active", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{}}`)
	})
	mux.HandleFunc("GET /api/session/{id}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":{"id":%q}}`, r.PathValue("id"))
	})
	mux.HandleFunc("GET /api/permission/request", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []map[string]any
		for id, sess := range s.pending {
			out = append(out, map[string]any{"id": id, "sessionID": sess, "action": "shell", "resources": []string{"make"}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": out})
	})
	mux.HandleFunc("GET /api/session/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var raws []json.RawMessage
		for i := len(s.messages) - 1; i >= 0; i-- {
			raws = append(raws, json.RawMessage(s.messages[i]))
		}
		json.NewEncoder(w).Encode(map[string]any{"data": raws, "cursor": map[string]any{}})
	})
	mux.HandleFunc("POST /api/session/{id}/prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		appendLine("prompts", r.PathValue("id")+": "+body.Text)
		s.mu.Lock()
		s.n++
		msg, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("msg_%d", s.n), "type": "user",
			"time": map[string]any{"created": time.Now().UnixMilli()}, "text": body.Text})
		s.messages = append(s.messages, string(msg))
		s.mu.Unlock()
		io.WriteString(w, `{"data":{}}`)
		s.send(fmt.Sprintf(`{"type":"session.inbox.delivered","created":%d,"data":{"sessionID":%q}}`, time.Now().UnixMilli(), r.PathValue("id")))
		s.send(fmt.Sprintf(`{"type":"session.execution.started","created":%d,"data":{"sessionID":%q}}`, time.Now().UnixMilli(), r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/session/{id}/permission/{req}/reply", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Decision string `json:"decision"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		_, ok := s.pending[r.PathValue("req")]
		delete(s.pending, r.PathValue("req"))
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"_tag":"PermissionNotFoundError","message":"Permission request not found"}`)
			return
		}
		appendLine("replies", r.PathValue("id")+" "+r.PathValue("req")+" "+body.Decision)
		w.WriteHeader(http.StatusNoContent)
		s.send(fmt.Sprintf(`{"type":"permission.replied","created":%d,"data":{"sessionID":%q,"requestID":%q,"reply":%q}}`,
			time.Now().UnixMilli(), r.PathValue("id"), r.PathValue("req"), body.Decision))
	})
	mux.HandleFunc("GET /api/event", func(w http.ResponseWriter, r *http.Request) {
		c := make(chan string, 64)
		s.mu.Lock()
		s.subs[c] = true
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.subs, c); s.mu.Unlock() }()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"server.connected\",\"data\":{}}\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case ev := <-c:
				fmt.Fprintf(w, "data: %s\n\n", ev)
				w.(http.Flusher).Flush()
			case <-time.After(time.Second):
				io.WriteString(w, ": heartbeat\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	auth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != opencode.User || p != password || password == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	go http.Serve(l, auth)
	go emitLoop(s)
	ended := make(chan string, 2)
	go func() { io.Copy(io.Discard, os.Stdin); ended <- "stdin closed" }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	go func() { <-sigs; ended <- "SIGTERM" }()
	appendLine("server.exit", <-ended)
}

// emitLoop sends each line appended to $DIR/emit as an event.
func emitLoop(s *server) {
	var off int64
	for {
		time.Sleep(20 * time.Millisecond)
		f, err := os.Open(filepath.Join(dir, "emit"))
		if err != nil {
			continue
		}
		f.Seek(off, io.SeekStart)
		br := bufio.NewReader(f)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			off += int64(len(line))
			if l := strings.TrimSpace(line); l != "" {
				s.send(l)
			}
		}
		f.Close()
	}
}
