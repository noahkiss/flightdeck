package main

import (
	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/catalog"
	"github.com/noahkiss/flightdeck/internal/peers"
)

// habConfigPath is the hab's own config file: -config, else $GFF_HAB_CONFIG, else gff/hab.toml
// in the user config folder (mesh.md 5.4).
func habConfigPath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	return catalog.DefaultConfigPath()
}

// catalogPeers is the peer list as the catalog reads it.
type catalogPeers struct{ m *peers.Manager }

func (c catalogPeers) List() api.PeerList { return c.m.List() }

func (c catalogPeers) Links() []catalog.Link {
	ls := c.m.Links()
	out := make([]catalog.Link, 0, len(ls))
	for _, l := range ls {
		cl := catalog.Link{Label: l.Label, DeviceID: l.DeviceID, Online: l.Online, Caps: l.Caps}
		if l.Conn != nil {
			cl.Caller = l.Conn
		}
		out = append(out, cl)
	}
	return out
}
