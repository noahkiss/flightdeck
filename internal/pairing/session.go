package pairing

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// session serves one session channel: only the flightdeck-pair subsystem, once.
func (w *Window) session(conn *ssh.ServerConn, c net.Conn, key ssh.PublicKey, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	fp := ssh.FingerprintSHA256(key)
	started := false
	run := make(chan struct{})
	for {
		select {
		case <-run:
			return
		case r, ok := <-reqs:
			if !ok {
				if started {
					<-run
				}
				return
			}
			var sub struct{ Name string }
			if r.Type == "subsystem" && !started && ssh.Unmarshal(r.Payload, &sub) == nil && sub.Name == Subsystem {
				started = true
				r.Reply(true, nil)
				go func() {
					defer close(run)
					w.pair(conn, c, key, fp, ch)
				}()
				continue
			}
			if r.WantReply {
				r.Reply(false, nil)
			}
			if !started {
				// A fixed reason: the request type is the client's string.
				w.refuse(conn.RemoteAddr(), fp, "request refused: only the "+Subsystem+" subsystem")
			}
		}
	}
}

// pair reads the request, checks the token, asks the owner and answers.
func (w *Window) pair(conn *ssh.ServerConn, c net.Conn, key ssh.PublicKey, fp string, ch ssh.Channel) {
	remote := conn.RemoteAddr()
	answer := func(a Answer) {
		b, _ := json.Marshal(a)
		ch.Write(append(b, '\n'))
	}
	end := func(a Answer) {
		answer(a)
		status := uint32(1)
		if a.State == "paired" {
			status = 0
		}
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		ch.CloseWrite()
	}
	line, err := readLine(ch, MaxRequest)
	if err != nil {
		w.refuse(remote, fp, "request not read")
		end(refused(ReasonRequest))
		return
	}
	req, reason, err := ParseRequest(line)
	if err == nil && subtle.ConstantTimeCompare(req.Token, w.token) != 1 {
		reason, err = ReasonToken, errors.New("wrong token")
	}
	if err != nil {
		w.refuse(remote, fp, err.Error())
		if reason == ReasonToken {
			// Counted per source address: past the limit that address is refused at accept.
			// The window stays open and a device waiting for the owner is never touched.
			w.mu.Lock()
			w.badTokens[sourceOf(remote)]++
			w.mu.Unlock()
		}
		end(refused(reason))
		return
	}
	w.mu.Lock()
	switch {
	case w.done:
		w.mu.Unlock()
		w.refuse(remote, fp, "token already used")
		end(refused(ReasonExpired))
		return
	case w.pending:
		w.mu.Unlock()
		w.refuse(remote, fp, "another device is waiting for the owner")
		end(refused(ReasonBusy))
		return
	}
	w.pending = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.pending = false
		w.mu.Unlock()
	}()

	// The owner has until the window closes; the connection lives a little longer, so the
	// phone still hears expired.
	c.SetDeadline(w.deadline.Add(5 * time.Second))
	answer(Answer{V: 1, State: "waiting"})
	dev := Device{Name: req.Name, Kind: req.Kind, Key: key, Fingerprint: fp, Remote: remote}
	// A device that leaves while the owner is asked ends the question: the owner must never
	// admit a key the phone did not stay to store. It counts as a no, so the token is spent.
	ask, cancel := context.WithCancel(w.ctx)
	defer cancel()
	go func() {
		conn.Wait()
		cancel()
	}()
	yes, err := w.cfg.Confirm(ask, dev)
	if err != nil && ask.Err() != nil && w.ctx.Err() == nil {
		w.refuse(remote, fp, "the device left before it was confirmed")
	}
	w.mu.Lock()
	if w.done || w.ctx.Err() != nil {
		// The window closed while the owner was asked.
		w.done = true
		w.mu.Unlock()
		end(refused(ReasonExpired))
		return
	}
	w.done = true
	// From here the outcome is this device's, whatever ends the window meanwhile (Wait).
	w.admitting = err == nil && yes
	w.mu.Unlock()
	if err != nil || !yes {
		end(refused(ReasonDeclined))
		w.finish(result{err: ErrDeclined})
		return
	}
	p, err := w.cfg.Admit(dev)
	if err != nil {
		w.cfg.Logf("could not save the key: %v", err)
		end(refused(ReasonFailed))
		w.finish(result{err: fmt.Errorf("%w: %v", ErrAdmitFail, err)})
		return
	}
	if p.SSH.Endpoints == nil {
		p.SSH.Endpoints = []string{}
	}
	if p.SSH.HostKeys == nil {
		p.SSH.HostKeys = []string{}
	}
	end(Answer{V: 1, State: "paired", Hab: &Hab{ID: w.offer.DeviceID, Label: w.cfg.Label}, DeviceID: fp,
		User: p.User, SSH: &p.SSH, ForcedCommand: p.ForcedCommand})
	w.finish(result{dev: &dev})
}

// readLine reads up to max bytes, until a newline, and returns the line without it.
func readLine(r io.Reader, max int) ([]byte, error) {
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for len(buf) < max {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return buf, nil
			}
			buf = append(buf, one[0])
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("request too long")
}
