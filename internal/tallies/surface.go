package tallies

import (
	"slices"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// HookSet is one harness and the native hook events its hook config posts.
type HookSet struct {
	Harness string
	Events  []string
}

// Surface is the closed key set of each tally: what a Tallies counts by name, and what
// gff-hab tallies --unused lists against.
type Surface struct {
	Methods  []string  // the API method table (what rpc.discover serves), then RouteOnly
	Events   []string  // the API event types
	Hooks    []HookSet // the hook events each harness's hook config posts
	Settings []string  // the settings registry
	Clients  []string  // the client kinds of the hello
	Errors   []string  // the API error kinds, and protocol for a JSON-RPC error with none
	Procs    []string  // the process kinds with health records
}

// ErrorProtocol is the error kind of a JSON-RPC protocol error, which has no API kind.
const ErrorProtocol = "protocol"

// ClientRoute is the client kind of a call on the pane route (POST /channel/<token>), which
// has no hello and so no client kind of the API.
const ClientRoute = "route"

// RouteOnly are the methods only the pane route takes: they are outside the API table
// (hab-logic-api.md 3.9, Route-only methods), and counted by name all the same.
func RouteOnly() []string {
	return slices.Concat(hook.AgentMethods, hook.PersonalMethods)
}

// Current is this build's surface: the method table and event types of package api, the hook
// events of package hook, the hab-side settings registry.
func Current() Surface {
	s := Surface{Procs: []string{"hab", "holder"}}
	for _, m := range api.Methods {
		s.Methods = append(s.Methods, m.Name)
	}
	s.Methods = append(s.Methods, RouteOnly()...)
	for _, e := range api.Events {
		s.Events = append(s.Events, string(e.Type))
	}
	s.Hooks = []HookSet{
		{Harness: detect.KindClaude, Events: append(append([]string(nil), hook.ClaudeEvents...), hook.StatusLineEvent)},
		{Harness: detect.KindCodex, Events: hook.CodexEvents},
	}
	for _, d := range settings.Defs {
		s.Settings = append(s.Settings, d.ID)
	}
	s.Clients = append(append(s.Clients, api.Enums["ClientKind"]...), ClientRoute)
	s.Errors = append(append(s.Errors, api.Enums["ErrorKind"]...), ErrorProtocol)
	return s
}

// HookKeys returns each hook event as harness/event, in counter order.
func (s Surface) HookKeys() []string {
	var out []string
	for _, h := range s.Hooks {
		for _, e := range h.Events {
			out = append(out, h.Harness+"/"+e)
		}
	}
	return out
}
