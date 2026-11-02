package pairing

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/noahkiss/flightdeck/internal/devicestore"
)

// Subsystem is the only request the pairing window serves.
const Subsystem = "flightdeck-pair"

// MaxRequest bounds the request line, newline included.
const MaxRequest = 4096

// Refusal reasons the window answers with (transport.md 6.1.2).
const (
	ReasonVersion  = "version"
	ReasonRequest  = "request"
	ReasonToken    = "token"
	ReasonDeclined = "declined"
	ReasonExpired  = "expired"
	ReasonBusy     = "busy"
	ReasonFailed   = "failed"
)

// Kinds a device may name; anything else is read as KindPhone.
const (
	KindPhone   = "phone"
	KindTablet  = "tablet"
	KindDesktop = "desktop"
)

// Request is a pairing request as the phone sent it, after ParseRequest.
type Request struct {
	Token []byte
	Name  string // cleaned
	Kind  string
}

type wireRequest struct {
	V     *int    `json:"v"`
	Token *string `json:"token"`
	Name  *string `json:"name"`
	Kind  string  `json:"kind"`
}

// ParseRequest reads one request line (its newline already cut). On a fault it returns the
// refusal reason to answer with. It parses bytes a client sent, before any check of who it is.
func ParseRequest(line []byte) (Request, string, error) {
	if len(line) >= MaxRequest {
		return Request{}, ReasonRequest, errors.New("request too long")
	}
	var w wireRequest
	d := json.NewDecoder(bytes.NewReader(line))
	if err := d.Decode(&w); err != nil {
		return Request{}, ReasonRequest, errors.New("request is not a JSON object")
	}
	if d.More() {
		return Request{}, ReasonRequest, errors.New("more than one JSON value")
	}
	if w.V == nil {
		return Request{}, ReasonRequest, errors.New("no version")
	}
	if *w.V != 1 {
		return Request{}, ReasonVersion, errors.New("unknown version")
	}
	if w.Token == nil {
		return Request{}, ReasonRequest, errors.New("no token")
	}
	tok, err := DecodeToken(*w.Token)
	if err != nil {
		// A malformed token is a wrong token: it counts toward the window's limit.
		return Request{}, ReasonToken, err
	}
	if w.Name == nil {
		return Request{}, ReasonRequest, errors.New("no name")
	}
	name := devicestore.CleanName(*w.Name)
	if name == "" {
		return Request{}, ReasonRequest, errors.New("empty name")
	}
	kind := w.Kind
	if kind != KindTablet && kind != KindDesktop {
		kind = KindPhone
	}
	return Request{Token: tok, Name: name, Kind: kind}, "", nil
}

// SSHInfo tells the phone how to reach the system sshd after pairing.
type SSHInfo struct {
	Endpoints []string `json:"endpoints"`
	HostKeys  []string `json:"host_keys"`
}

// Hab names the hab in a paired answer.
type Hab struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// Answer is one answer line.
type Answer struct {
	V             int      `json:"v"`
	State         string   `json:"state"`
	Reason        string   `json:"reason,omitempty"`
	Hab           *Hab     `json:"hab,omitempty"`
	DeviceID      string   `json:"device_id,omitempty"`
	User          string   `json:"user,omitempty"`
	SSH           *SSHInfo `json:"ssh,omitempty"`
	ForcedCommand bool     `json:"forced_command,omitempty"`
}

func refused(reason string) Answer { return Answer{V: 1, State: "refused", Reason: reason} }
