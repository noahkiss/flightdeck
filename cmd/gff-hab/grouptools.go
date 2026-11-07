package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// The agent kit's group tools (groups.md 15.2): the coordinator proposes, amends, closes and
// retries; any member reads the record, reports to the coordinator and notes its item.

func init() { actionTools = append(actionTools, groupTools...) }

var argGroup = toolArg{Name: "group", Type: "string", Doc: "The group id from group_create.", Required: true}

var groupTools = []kitTool{
	{Name: "group_create", Action: true, Doc: "Propose an agent group for a task with several separable items: one lead per item, each in its own worktree and branch (group/<group>/<item>), on any host and harness, with a reviewer in another harness. Flightdeck checks the plan, files a Logbook plan with one task per item, and asks the owner one confirmation for the whole plan while the owner's setting is on; the tool waits up to 45 s, then says it is still waiting, and the answer reaches you as a message from Flightdeck. Nothing starts before it. Then leads start in plan order, at most group.max_leads at once. Do not use a group for one change: a single session costs less. You are the coordinator: reports from members come to you as messages from Flightdeck. Run it from your Flightdeck pane.",
		Args: []toolArg{{Name: "plan", Type: "string", Required: true, Doc: `The plan as JSON: {"title": "...", "brief": "...", "project": "<name or remote from projects>", "items": [{"id": "api", "title": "...", "brief": "...", "after": "<item it stacks on>", "review": "on"}], "members": [{"role": "lead", "item": "api", "host": "<label, absent for this host>", "harness": "claude", "model": "...", "name": "..."}], "lane": {"target": "main"}}. One lead per item; reviewers and workers are optional.`}},
		run: runGroupCreate},
	{Name: "group_status", Action: true, Doc: "Read an agent group's record: its state, each item's state and lead, each member's host, harness and agent state, the waits, and the reports waiting for the coordinator.",
		Args: []toolArg{argGroup}, run: runGroupStatus},
	{Name: "group_amend", Action: true, Doc: "Add items or members to your running group: one confirmation for the change, by the same setting as the plan. Use it for a new item, a new host or a new harness. Coordinator only.",
		Args: []toolArg{argGroup, {Name: "change", Type: "string", Required: true, Doc: `The items and members to add, as JSON: {"items": [...], "members": [...]}, in the plan's form.`}},
		run: runGroupAmend},
	{Name: "group_close", Action: true, Doc: "End your group: nothing new starts, and its idle members on the home host are closed when the owner's group.close_members is on. Coordinator only.",
		Args: []toolArg{argGroup}, run: runGroupClose},
	{Name: "group_retry", Action: true, Doc: "Start a member of your group again: a lead that failed to start, or was closed, or whose worktree setup waited for the owner's yes. It asks nothing: the plan's confirmation covers it. Coordinator only.",
		Args: []toolArg{argGroup, {Name: "member", Type: "string", Required: true, Doc: "The member's name, from group_status."}},
		run: runGroupRetry},
	{Name: "group_report", Action: true, Doc: "Report to your group's coordinator: one line or a short text, such as done, blocked on X, or a question. Flightdeck delivers it when the coordinator is idle, or keeps it until the coordinator is resumed. Use it rather than channel_send for the group's news.",
		Args: []toolArg{argGroup, {Name: "text", Type: "string", Required: true, Doc: "At most 4000 characters."}},
		run: runGroupReport},
	{Name: "group_note", Action: true, Doc: "Write a progress or hand-off note on your item's Logbook task. Flightdeck writes it, so it lands at once whatever the owner's approval setting.",
		Args: []toolArg{argGroup, {Name: "item", Type: "string", Required: true, Doc: "The item id."}, {Name: "text", Type: "string", Required: true, Doc: "At most 4000 characters."}},
		run: runGroupNote},
}

// groupWait is how long group_create and group_amend wait for the answer, as start_session.
const groupWait = 45 * time.Second

func runGroupCreate(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var plan api.GroupPlan
	if err := json.Unmarshal([]byte(a.str("plan")), &plan); err != nil {
		return nil, "", control.Errorf(api.ErrorKindInvalid, "plan is not JSON in the plan's form: %v", err)
	}
	var r api.GroupCreated
	if err := c.Call(ctx, "group.create", api.GroupCreateParams{Plan: plan, WaitMS: int(groupWait / time.Millisecond)}, &r); err != nil {
		return nil, "", err
	}
	return r, groupCreatedText(r), nil
}

func runGroupAmend(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var change api.GroupPlan
	if err := json.Unmarshal([]byte(a.str("change")), &change); err != nil {
		return nil, "", control.Errorf(api.ErrorKindInvalid, "change is not JSON in the plan's form: %v", err)
	}
	var r api.GroupCreated
	if err := c.Call(ctx, "group.amend", api.GroupAmendParams{Group: a.str("group"), Change: change, WaitMS: int(groupWait / time.Millisecond)}, &r); err != nil {
		return nil, "", err
	}
	return r, groupCreatedText(r), nil
}

func runGroupStatus(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.GroupResult
	if err := c.Call(ctx, "group.get", api.GroupRef{Group: a.str("group")}, &r); err != nil {
		return nil, "", err
	}
	return r, groupText(r.Group), nil
}

func runGroupClose(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.GroupResult
	if err := c.Call(ctx, "group.close", api.GroupRef{Group: a.str("group")}, &r); err != nil {
		return nil, "", err
	}
	return r, groupText(r.Group), nil
}

func runGroupRetry(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.GroupResult
	if err := c.Call(ctx, "group.retry", api.GroupRetryParams{Group: a.str("group"), Member: a.str("member")}, &r); err != nil {
		return nil, "", err
	}
	return r, groupText(r.Group), nil
}

func runGroupReport(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.GroupReported
	if err := c.Call(ctx, "group.report", api.GroupReportParams{Group: a.str("group"), Text: a.str("text")}, &r); err != nil {
		return nil, "", err
	}
	if r.State == "waiting" {
		return r, "The coordinator is not running; the report waits for its resume.\n", nil
	}
	return r, "Sent to the coordinator.\n", nil
}

func runGroupNote(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.Empty
	if err := c.Call(ctx, "group.note", api.GroupNoteParams{Group: a.str("group"), Item: a.str("item"), Text: a.str("text")}, &r); err != nil {
		return nil, "", err
	}
	return r, "Noted on the item's task.\n", nil
}

func groupCreatedText(r api.GroupCreated) string {
	var b strings.Builder
	if c := r.Confirmation; c != nil {
		b.WriteString(groupConfirmText(*c))
	}
	b.WriteString(groupText(r.Group))
	return b.String()
}

// groupConfirmText says where a plan's confirmation stands.
func groupConfirmText(c api.Confirmation) string {
	switch c.State {
	case "open":
		return fmt.Sprintf("Still waiting for the owner to confirm the plan. Confirmation %s. Do not ask again: the answer reaches you as a message from Flightdeck, and group_status shows where the group stands.\n", c.ID)
	case "running", "done":
		if c.Record {
			return "The plan started at once, with no confirmation asked.\n"
		}
		return fmt.Sprintf("The owner confirmed the plan (confirmation %s).\n", c.ID)
	case "cancelled":
		return "The owner cancelled the plan. Nothing started.\n"
	case "expired":
		return "The plan's confirmation expired. Nothing started.\n"
	}
	return fmt.Sprintf("The plan's confirmation is %s: %s\n", c.State, c.Reason)
}

// groupText is a group's record as an agent reads it.
func groupText(g api.Group) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Group %s: %s. State %s. Coordinator %s.\n", g.ID, g.Title, g.State, g.Coordinator.Address)
	if g.Pause.User != "" || g.Pause.Usage != "" {
		fmt.Fprintf(&b, "Paused: user %v, usage %v.\n", g.Pause.User != "", g.Pause.Usage != "")
	}
	b.WriteString("Items:\n")
	for _, it := range g.Items {
		fmt.Fprintf(&b, "- %s (%s): %s, lead %s, branch %s, task %s", it.ID, it.Title, it.State, it.Lead, it.Branch, it.Task)
		if it.Reason != "" {
			fmt.Fprintf(&b, ": %s", it.Reason)
		}
		b.WriteString("\n")
	}
	b.WriteString("Members:\n")
	for _, m := range g.Members {
		fmt.Fprintf(&b, "- %s: %s of %s on %s (%s), %s", m.Name, m.Role, m.Item, m.Host, m.Harness, m.State)
		if m.Agent != "" {
			fmt.Fprintf(&b, ", agent %s", m.Agent)
		}
		if m.Reason != "" {
			fmt.Fprintf(&b, ": %s", m.Reason)
		}
		b.WriteString("\n")
	}
	for _, w := range g.Waits {
		fmt.Fprintf(&b, "Waiting: %s\n", w)
	}
	if n := len(g.Inbox); n > 0 {
		fmt.Fprintf(&b, "%d reports wait for the coordinator.\n", n)
	}
	return b.String()
}
