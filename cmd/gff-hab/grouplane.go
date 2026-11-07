package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/heavy"
	"github.com/noahkiss/flightdeck/internal/lane"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/worktree"
)

// The merge lane on the home hab (groups.md 7.3 to 7.5): one item at a time per group, one run
// at a time per repository, in the order items reached ready, never before the item an item
// stacks on. internal/lane runs git and the project's commands; this file writes the outcome to
// the record, tells the lead and the coordinator, removes the worktrees of landed items whose
// sessions ended, and ends the group when every item is landed or dropped.

// laneRetry is how long the lane waits after a run that could not run (no remote, a fetch that
// failed) before it tries again.
const laneRetry = time.Minute

// laneState is the lane's part of the group service.
type laneState struct {
	running map[string]bool // a lane pass runs for the group
	again   map[string]bool
	repos   map[string]*sync.Mutex // one lane run at a time per repository
	tried   map[string]bool        // worktrees whose removal was refused: tried again at the end
	armed   map[string]bool        // a retry is armed for the group: one at a time
	runs    map[string]*laneRun    // the run in flight per group: the end of a group stops it
	ctx     context.Context        // ends at Stop
	cancel  context.CancelFunc
}

// laneRun is one lane run in flight.
type laneRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *groupsService) laneInit() {
	s.lane.running, s.lane.again = map[string]bool{}, map[string]bool{}
	s.lane.repos, s.lane.tried = map[string]*sync.Mutex{}, map[string]bool{}
	s.lane.armed, s.lane.runs = map[string]bool{}, map[string]*laneRun{}
	s.lane.ctx, s.lane.cancel = context.WithCancel(context.Background())
}

// laneKick runs the group's lane, or asks the running pass for another.
func (s *groupsService) laneKick(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.lane.running[id] {
		s.lane.again[id] = true
		return
	}
	s.lane.running[id] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			s.lanePass(id)
			s.mu.Lock()
			if !s.lane.again[id] || s.closed {
				delete(s.lane.running, id)
				delete(s.lane.again, id)
				s.mu.Unlock()
				return
			}
			s.lane.again[id] = false
			s.mu.Unlock()
		}
	}()
}

// laneLaterLocked kicks the lane again after the retry time, unless the service stops first.
// One retry at a time is armed per group: every scheduler pass kicks the lane, and each kick
// during a wait would otherwise start one more retry.
func (s *groupsService) laneLaterLocked(id string) {
	if s.closed || s.lane.armed[id] {
		return
	}
	s.lane.armed[id] = true
	d := s.laneRetryAfter()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			s.mu.Lock()
			delete(s.lane.armed, id)
			s.mu.Unlock()
			s.laneKick(id)
		case <-s.stop:
		}
	}()
}

func (s *groupsService) repoLock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lane.repos[key]
	if l == nil {
		l = &sync.Mutex{}
		s.lane.repos[key] = l
	}
	return l
}

// laneJob is what one run needs, read under the lock.
type laneJob struct {
	lane.Job
	repo string // the repository's lock key
	err  string // why it cannot run: the lane waits
}

// laneJobLocked builds the run from the record and a catalog read before the lock: the
// catalog reads the groups section under s.mu.
func (s *groupsService) laneJobLocked(cl api.CatalogList, g *api.Group, it api.GroupItem) laneJob {
	proj, ok := project(cl, g.Plan.Project)
	if !ok {
		return laneJob{err: fmt.Sprintf("project %s is no longer in the catalog", g.Plan.Project)}
	}
	src := folderOn(proj, s.cfg.self)
	if src == "" {
		return laneJob{err: "the project is not set up on the group's home hab, where the lane runs"}
	}
	key := proj.Remote
	if key == "" {
		key = src
	}
	sum := sha256.Sum256([]byte(key))
	j := laneJob{repo: key, Job: lane.Job{
		Source: src, Dir: filepath.Join(s.cfg.laneDir, hex.EncodeToString(sum[:8])), Identity: proj.Remote,
		Target: g.Plan.Lane.Target, Branch: it.Branch, Land: it.Land, Upstreams: groups.Upstreams(*g, it),
		Prior: groups.Prior(it), Tries: s.cfg.keys.LaneTries, OutputLines: s.cfg.keys.FixOutputLines,
		Git: s.cfg.laneGit,
	}}
	if s.cfg.heavy != nil {
		cmd := "lane checks: group " + g.ID + " item " + it.ID
		j.Heavy = func(ctx context.Context) (func(), error) { return s.cfg.heavy(ctx, cmd) }
	}
	return j
}

// lanePass lands the group's items one at a time while there is one to land.
func (s *groupsService) lanePass(id string) {
	for {
		cl := s.cfg.catalog()
		s.mu.Lock()
		g := s.groups[id]
		if g == nil || s.closed || g.State != groups.StateRunning || g.Lane.Stopped != "" || s.laneDir() == "" {
			s.mu.Unlock()
			return
		}
		item := groups.NextLand(*g)
		if item == "" {
			s.mu.Unlock()
			return
		}
		ii := groups.ItemIndex(*g, item)
		it := &g.Items[ii]
		j := s.laneJobLocked(cl, g, *it)
		if j.err != "" {
			if g.Lane.Waits != j.err {
				g.Lane.Waits = j.err
				s.saveLocked(g)
			}
			s.laneLaterLocked(id)
			s.mu.Unlock()
			return
		}
		it.State, g.Lane.Landing = groups.ItemLanding, item
		s.saveLocked(g)
		ctx, cancel := context.WithCancel(s.lane.ctx)
		run := &laneRun{cancel: cancel, done: make(chan struct{})}
		s.lane.runs[id] = run
		s.mu.Unlock()

		j.Saved = func(a lane.Attempt) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			g := s.groups[id]
			if g == nil {
				return errors.New("the group is gone")
			}
			it := &g.Items[ii]
			it.Attempts = append(it.Attempts, api.GroupLaneAttempt{At: stampT(s.cfg.now()), Target: a.Target, Rebased: a.Rebased, Outcome: "pushed", Step: lane.StepPush})
			return s.saveLocked(g)
		}
		l := s.repoLock(j.repo)
		l.Lock()
		res := lane.Run(ctx, j.Job)
		l.Unlock()
		ended := ctx.Err() != nil // the end of the group, or Stop, stopped the run
		cancel()
		s.mu.Lock()
		delete(s.lane.runs, id)
		s.mu.Unlock()
		if s.lane.ctx.Err() != nil {
			close(run.done)
			return // the hab stops: the item stays landing, and the next start runs it again
		}
		if ended && res.Outcome != lane.Landed {
			s.laneEnded(id, ii, item) // the group ended during the run: nothing more lands
			close(run.done)
			return
		}
		cont := s.laneDone(id, ii, item, res)
		close(run.done)
		if !cont {
			return
		}
	}
}

// laneEnded puts an item whose run the end of its group stopped back to ready.
func (s *groupsService) laneEnded(id string, ii int, item string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[id]
	if g == nil || ii >= len(g.Items) || g.Items[ii].ID != item {
		return
	}
	if g.Items[ii].State == groups.ItemLanding {
		g.Items[ii].State = groups.ItemReady
	}
	if g.Lane.Landing == item {
		g.Lane.Landing = ""
	}
	s.saveLocked(g)
}

// laneStop stops the group's run in flight, if any, and waits for its outcome to be written.
func (s *groupsService) laneStop(id string) {
	s.mu.Lock()
	run := s.lane.runs[id]
	s.mu.Unlock()
	if run != nil {
		run.cancel()
		<-run.done
	}
}

// laneDone writes a run's outcome. It reports whether the lane goes on with the next item.
func (s *groupsService) laneDone(id string, ii int, item string, res lane.Result) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[id]
	if g == nil || ii >= len(g.Items) || g.Items[ii].ID != item {
		return false
	}
	it := &g.Items[ii]
	target := res.Target
	if target == "" {
		target = "the target"
	}
	if res.Outcome != lane.Unavailable {
		// A run that could not run is retried each minute: it records no attempt, so the
		// record does not grow while the lane waits.
		at := api.GroupLaneAttempt{At: stampT(s.cfg.now()), Outcome: string(res.Outcome), Step: res.Step}
		if res.Outcome == lane.Landed {
			at.Rebased = res.Commit
		}
		it.Attempts = append(it.Attempts, at)
	}
	lead := groups.MemberIndex(*g, it.Lead)
	noCI := ""
	if res.NoCI {
		noCI = " (no CI)"
	}
	cont := true
	switch res.Outcome {
	case lane.Landed:
		it.State, it.Landed, it.NoCI, it.Reason = groups.ItemLanded, res.Commit, res.NoCI, ""
		g.Lane.NoCI, g.Lane.Waits = res.NoCI, ""
		groups.Dequeue(g, item)
		if lead >= 0 {
			s.memberNoteLocked(g, g.Members[lead], fmt.Sprintf("Group %s, item %s: landed on %s at %s%s. Your session keeps running: the coordinator may still ask about the work.", g.ID, it.ID, target, groups.Short7(res.Commit), noCI))
		}
		s.tellLocked(g, fmt.Sprintf("item %s landed on %s at %s%s.", it.ID, target, groups.Short7(res.Commit), noCI), "Flightdeck")
		if it.Task != "" {
			go s.setStatus([]string{it.Task}, "done", "")
		}
		go s.kick(g.ID) // its lead slot is free (groups.md 4.3, 7.3 step 7)
		if groups.Finished(*g) {
			go s.end(g.ID, "The group ended: every item is landed or dropped.")
			cont = false
		}
	case lane.Back:
		it.State, it.Land, it.Reason = groups.ItemFix, "", "the lane sent it back at "+res.Step
		groups.Dequeue(g, item)
		what := "Rebase on " + target + ", fix it, and submit again."
		if res.Step == lane.StepMoving {
			what = "The target keeps moving: rebase on " + target + " and submit again, or ask the coordinator."
		}
		text := fmt.Sprintf("Group %s, item %s: the lane sent it back at the %s step. %s", g.ID, it.ID, res.Step, what)
		if res.Output != "" {
			text += "\n\nThe output:\n" + res.Output
		}
		if lead >= 0 {
			s.memberNoteLocked(g, g.Members[lead], text)
		}
		line := fmt.Sprintf("item %s went back to its lead from the lane's %s step.", it.ID, res.Step)
		if res.Step == lane.StepMoving {
			line = fmt.Sprintf("item %s went back: main keeps moving (%d tries).", it.ID, res.Tries)
		}
		s.tellLocked(g, line, "Flightdeck")
	case lane.Stopped:
		// A rule refused the push: every item would fail the same way (groups.md 7.4).
		it.State = groups.ItemReady
		g.Lane.Landing = ""
		g.Lane.Stopped = fmt.Sprintf("the remote refused the %s push of item %s for a rule: %s", res.Step, it.ID, firstLine(res.Output))
		s.tellLocked(g, "The lane stopped: "+g.Lane.Stopped+". Fix the rule or the permission, then group_retry with lane starts it again.", "Flightdeck")
		cont = false
	default: // unavailable: tried again later
		it.State = groups.ItemReady
		g.Lane.Landing = ""
		if g.Lane.Waits != res.Output {
			s.cfg.logf("groups: %s: the lane waits: %s", g.ID, res.Output)
		}
		g.Lane.Waits = res.Output
		s.laneLaterLocked(g.ID)
		cont = false
	}
	s.saveLocked(g)
	return cont
}

func (s *groupsService) laneRetryAfter() time.Duration {
	if s.cfg.laneRetry > 0 {
		return s.cfg.laneRetry
	}
	return laneRetry
}

func (s *groupsService) laneDir() string { return s.cfg.laneDir }

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// inLane reports whether an item is in the lane or past it: a lead that closes then stalls
// nothing (groups.md 7.3 step 7).
func inLane(st api.GroupItemState) bool {
	switch st {
	case groups.ItemReady, groups.ItemLanding, groups.ItemLanded, groups.ItemDropped:
		return true
	}
	return false
}

// --- worktrees after a land, and the end of a group (groups.md 7.3 step 7, 7.5) ---

// sweepLocked starts the removal of the worktrees of landed items' members on this hab whose
// sessions ended. A worktree with uncommitted changes stays, and the end lists it.
func (s *groupsService) sweepLocked(g *api.Group) {
	for _, m := range g.Members {
		if m.Device != s.cfg.self || m.Folder == "" || m.WorktreeRemoved || m.Session == "" {
			continue
		}
		if ii := groups.ItemIndex(*g, m.Item); ii < 0 || g.Items[ii].State != groups.ItemLanded {
			continue
		}
		if !s.sessionEnded(m) {
			continue
		}
		key := g.ID + "/" + m.Name
		if s.lane.tried[key] {
			continue
		}
		s.lane.tried[key] = true
		gid, name, folder := g.ID, m.Name, m.Folder
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.removeWorktree(folder); err != nil {
				s.cfg.logf("groups: %s: the worktree of %s stays: %v", gid, name, err)
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if g := s.groups[gid]; g != nil {
				if i := groups.MemberIndex(*g, name); i >= 0 {
					g.Members[i].WorktreeRemoved = true
					s.saveLocked(g)
				}
			}
		}()
	}
}

// sessionEnded reports whether a member's session on this hab has ended: its record is gone or
// its program exited.
func (s *groupsService) sessionEnded(m api.GroupMember) bool {
	if m.State == groups.MemberExited {
		return true
	}
	ss, ok := s.cfg.store.Session(m.Session)
	return !ok || ss.State == "exited"
}

// removeWorktree removes a worktree on this hab with no force.
func (s *groupsService) removeWorktree(folder string) error {
	if s.cfg.removeWorktree == nil {
		return errors.New("no worktree removal on this hab")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := s.cfg.removeWorktree(ctx, api.WorktreeRemoveParams{WorktreeProject: api.WorktreeProject{ProjectDir: folder}, Path: folder})
	return err
}

// end ends a group (groups.md 7.5): it closes idle members on this hab, removes the worktrees
// of landed items whose sessions ended, lists the leftovers on the record and in one message to
// the coordinator, and marks the Logbook plan done, or dropped when nothing landed. It removes
// nothing with uncommitted changes. head is the message's first sentence, before the result;
// "" sends no message (the coordinator's own close).
func (s *groupsService) end(id, head string) (api.Group, error) {
	closeIdle := s.boolSetting(settings.GroupCloseMembers, true)
	s.mu.Lock()
	g := s.groups[id]
	if g == nil {
		s.mu.Unlock()
		return api.Group{}, fmt.Errorf("no group %s on this hab", id)
	}
	if !groups.Live(*g) || g.State == groups.StateConfirming || g.State == groups.StateEnding {
		st := g.State
		s.mu.Unlock()
		return api.Group{}, fmt.Errorf("group %s is %s", id, st)
	}
	g.State = groups.StateEnding
	var panes []string
	closing := map[string]bool{}
	for _, m := range g.Members {
		if m.Device != s.cfg.self || m.State != groups.MemberRunning || !closeIdle {
			continue
		}
		if m.Agent != "idle" && m.Agent != "" && m.Agent != api.AgentStatusExited {
			continue // a working member is left running, and listed
		}
		p := s.panes[g.ID+"/"+m.Name]
		if p == "" {
			if ss, ok := s.cfg.store.Session(m.Session); ok {
				p = ss.Pane
			}
		}
		if p != "" {
			panes, closing[m.Name] = append(panes, p), true
		}
	}
	s.saveLocked(g)
	s.mu.Unlock()
	// No more lands once the group ends (groups.md 7.5): a run in flight stops, and its outcome
	// is on the record before the leftovers are listed. A land push already sent counts.
	s.laneStop(id)
	s.mu.Lock()
	if g = s.groups[id]; g == nil {
		s.mu.Unlock()
		return api.Group{}, fmt.Errorf("no group %s on this hab", id)
	}
	snap := copyGroup(*g)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, p := range panes {
		if s.cfg.closePane != nil {
			if err := s.cfg.closePane(ctx, p); err != nil {
				s.cfg.logf("groups: %s: closing pane %s: %v", id, p, err)
			}
		}
	}
	var left []string
	removed := map[string]bool{}
	for _, m := range snap.Members {
		ii := groups.ItemIndex(snap, m.Item)
		landed := ii >= 0 && snap.Items[ii].State == groups.ItemLanded
		ended := closing[m.Name] || m.State == groups.MemberExited || m.State == groups.MemberFailed ||
			(m.Device == s.cfg.self && m.Session != "" && s.sessionEnded(m))
		switch {
		case m.Folder == "" || m.WorktreeRemoved:
		case !ended:
			left = append(left, fmt.Sprintf("member %s on %s is still running, in %s", m.Name, m.Host, m.Folder))
		case landed && m.Device == s.cfg.self:
			if err := s.removeWorktree(m.Folder); err != nil {
				left = append(left, fmt.Sprintf("worktree %s on %s (%s): %s", m.Folder, m.Host, m.Name, errText(err)))
			} else {
				removed[m.Name] = true
			}
		case landed:
			left = append(left, fmt.Sprintf("worktree %s on %s (%s)", m.Folder, m.Host, m.Name))
		default:
			dirty := ""
			if m.Device == s.cfg.self {
				if d, err := (worktree.Git{}).Dirty(ctx, m.Folder); err == nil && d {
					dirty = ", with uncommitted changes"
				}
			}
			left = append(left, fmt.Sprintf("worktree %s on %s of item %s, not landed%s", m.Folder, m.Host, m.Item, dirty))
		}
	}
	landedAny := false
	for _, it := range snap.Items {
		if it.State == groups.ItemLanded {
			landedAny = true
			continue
		}
		left = append(left, fmt.Sprintf("branch %s, not landed", it.Branch))
	}
	left = append(left, s.buildFolders(ctx, snap)...)

	s.mu.Lock()
	defer s.mu.Unlock()
	g = s.groups[id]
	if g == nil {
		return api.Group{}, fmt.Errorf("no group %s on this hab", id)
	}
	for i := range g.Members {
		if removed[g.Members[i].Name] {
			g.Members[i].WorktreeRemoved = true
		}
	}
	for _, l := range left {
		g.Leftovers = appendOnce(g.Leftovers, l)
	}
	g.State, g.Ended = groups.StateEnded, stampT(s.cfg.now())
	g.Lane.Landing = ""
	status := "done"
	if !landedAny {
		status = "dropped"
	}
	if g.PlanRecord != "" {
		go s.setStatus([]string{g.PlanRecord}, status, "")
	}
	if head != "" {
		msg := head + " " + groupEndText(*g)
		if len(g.Leftovers) > 0 {
			msg += " Left over:\n- " + strings.Join(g.Leftovers, "\n- ")
		} else {
			msg += " Nothing is left over."
		}
		s.tellLocked(g, msg, "Flightdeck")
	}
	s.saveLocked(g)
	return copyGroup(*g), nil
}

// buildFolders lists the per-run build folders the project's flightdeck.json names on the
// target (groups.md 7.5), from the lane's repository on this hab.
func (s *groupsService) buildFolders(ctx context.Context, g api.Group) []string {
	proj, ok := project(s.cfg.catalog(), g.Plan.Project)
	if !ok {
		return nil
	}
	src := folderOn(proj, s.cfg.self)
	if src == "" {
		return nil
	}
	rev := "HEAD"
	if g.Plan.Lane.Target != "" {
		if r := s.cfg.laneGit.RemoteFor(ctx, src, proj.Remote); r != "" {
			rev = "refs/remotes/" + r + "/" + g.Plan.Lane.Target
		}
	}
	cfg, err := lane.TargetConfig(ctx, s.cfg.laneGit, src, rev)
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range cfg.BuildFolders {
		out = append(out, "build folder "+f+" (flightdeck.json lane.build_folders)")
	}
	slices.Sort(out)
	return out
}

// heavyHold takes this hab's heavy-run lock for the lane's checks, waiting first come, first
// served (groups.md 8.1), and returns its release. The ticket goes when ctx ends.
func heavyHold(ctx context.Context, l *heavy.Lock, command string) (func(), error) {
	hctx, cancel := context.WithCancel(ctx)
	t, err := l.Take(hctx, api.HeavyTakeParams{Command: command})
	if err != nil {
		cancel()
		return nil, err
	}
	for t.State != api.HeavyStateHolding {
		if t, err = l.Wait(hctx, t.Ticket); err != nil || ctx.Err() != nil {
			cancel()
			if err == nil {
				err = ctx.Err()
			}
			return nil, err
		}
	}
	return func() {
		l.Release(api.HeavyReleaseParams{Ticket: t.Ticket})
		cancel()
	}, nil
}
