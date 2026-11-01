package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Event is one event from the server's stream.
type Event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"` // ms since the Unix epoch, the server's clock
	// Location is the folder the event happened in, when it names one.
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Data json.RawMessage `json:"data"`
}

// SessionID returns the data's sessionID, if it has one.
func (e Event) SessionID() string {
	var d struct {
		SessionID string `json:"sessionID"`
	}
	json.Unmarshal(e.Data, &d)
	return d.SessionID
}

// maxEvent bounds one event. A longer one ends the stream with an error: the caller reconnects
// and reads state again, so nothing is lost but the event.
const maxEvent = 8 << 20

// StreamIdle is how long the stream may stay silent before Events gives up on it. The server
// sends a heartbeat comment far more often.
var StreamIdle = 60 * time.Second

// ErrStreamIdle: the server sent nothing, not even a heartbeat, for StreamIdle.
var ErrStreamIdle = errors.New("opencode event stream: no data or heartbeat")

// Events subscribes to the server's event stream, filtered to types (all when empty), and
// calls fn for each event in order until ctx ends or the stream fails. connected is called
// once the server accepted the subscription, before any event, so the caller can read state
// knowing no later change is missed. Events always returns an error: ctx's, or why the stream
// ended.
func (c *Client) Events(ctx context.Context, types []string, connected func(), fn func(Event)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	q := url.Values{}
	for _, t := range types {
		q.Add("types", t)
	}
	resp, err := c.request(ctx, "event.subscribe", http.MethodGet, "/api/event", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	idle := time.AfterFunc(StreamIdle, func() { cancel(ErrStreamIdle) })
	defer idle.Stop()
	if connected != nil {
		connected()
	}
	err = readSSE(resp.Body, func() { idle.Reset(StreamIdle) }, func(data []byte) error {
		var e Event
		if err := json.Unmarshal(data, &e); err != nil {
			return fmt.Errorf("opencode event: %w", err)
		}
		if e.Type != "" && e.Type != "server.connected" {
			fn(e)
		}
		return nil
	})
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err == nil {
		err = io.EOF
	}
	return fmt.Errorf("opencode event stream: %w", err)
}

// readSSE reads a text/event-stream and calls fn with the data of each event. Comment lines
// (heartbeats) and fields other than data are skipped; seen is called for every line.
func readSSE(r io.Reader, seen func(), fn func(data []byte) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var data []byte
	for {
		line, err := readLine(br)
		if err != nil {
			return err
		}
		seen()
		switch {
		case len(line) == 0:
			if len(data) > 0 {
				if err := fn(data); err != nil {
					return err
				}
				data = data[:0]
			}
		case line[0] == ':':
		case bytes.HasPrefix(line, []byte("data:")):
			v := bytes.TrimPrefix(line[5:], []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			if len(data)+len(v) > maxEvent {
				return fmt.Errorf("an event over %d bytes", maxEvent)
			}
			data = append(data, v...)
		}
	}
}

// readLine reads one line without its CR LF or LF, up to maxEvent bytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(line)+len(chunk) > maxEvent {
			return nil, fmt.Errorf("a line over %d bytes", maxEvent)
		}
		line = append(line, chunk...)
		if !isPrefix {
			return line, nil
		}
	}
}
