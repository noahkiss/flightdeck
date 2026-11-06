package control

import (
	"context"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// peerMark is a Channels whose Wait reports whether its call came over a peer link.
type peerMark struct{}

func (peerMark) Send(context.Context, api.ChannelSendParams) (api.ChannelSent, error) {
	return api.ChannelSent{}, nil
}
func (peerMark) Status(context.Context, api.ChannelStatusParams) (api.ChannelStatusResult, error) {
	return api.ChannelStatusResult{}, nil
}
func (peerMark) Wait(ctx context.Context, p api.ChannelWaitParams) (api.ChannelWaitResult, error) {
	st := api.AgentStatusIdle
	if PeerCall(ctx) {
		st = api.AgentStatusExited
	}
	return api.ChannelWaitResult{Agent: &api.ChannelAgent{Session: p.Agent, State: st}}, nil
}
func (peerMark) Push(api.PeerHello, api.MailPushParams) (api.MailPushResult, error) {
	return api.MailPushResult{}, nil
}
func (peerMark) Pull(api.PeerHello, api.MailPullParams) (api.MailPullResult, error) {
	return api.MailPullResult{}, nil
}

// TestChannelWaitPeerCall: a channel.wait over a peer link carries the peer-call mark, so it is
// never forwarded again; one from any other client does not (mesh.md 6.6).
func TestChannelWaitPeerCall(t *testing.T) {
	path := socketPath(t)
	startHab(t, "", path, func(c *Config) { c.Channels = peerMark{} })

	peer := dial(t, path)
	if _, err := peer.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}}); err != nil {
		t.Fatal(err)
	}
	var r api.ChannelWaitResult
	if err := peer.Call(ctx(t), "channel.wait", api.ChannelWaitParams{Agent: "bob"}, &r); err != nil || r.Agent == nil || r.Agent.State != api.AgentStatusExited {
		t.Fatalf("over a peer link: %+v %v", r, err)
	}

	cl := dial(t, path)
	hello(t, cl, "")
	r = api.ChannelWaitResult{}
	if err := cl.Call(ctx(t), "channel.wait", api.ChannelWaitParams{Agent: "bob"}, &r); err != nil || r.Agent == nil || r.Agent.State != api.AgentStatusIdle {
		t.Fatalf("from a client: %+v %v", r, err)
	}
}
