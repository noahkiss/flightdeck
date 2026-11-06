package logbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Property tests of logbook.md 17 that stage 2 owns: P2, P3 and P6 here, P10 in p10_test.go.
// Each logs its seed; $GFF_LOGBOOK_SEED replays one. $GFF_LOGBOOK_NIGHTLY=1 sets the nightly
// counts, and $GFF_LOGBOOK_RUNS overrides any count.

// runs returns the run count: the per-push count, the nightly one, or $GFF_LOGBOOK_RUNS.
//
// Under -race the counts drop to a twentieth: the race build runs the same code paths for data
// races, about ten times slower, and make test runs the full per-push counts without it.
func runs(perPush, night int) int {
	n := perPush
	if nightly() {
		n = night
	}
	if raceEnabled {
		n = max(1, n/20)
	}
	return envInt("GFF_LOGBOOK_RUNS", n)
}

// world is a random history: replicas that write every op kind and exchange ops at random over a
// link that drops part of what it sends. Beside the writes it plays what peers do to a replica:
// creates of one id on two replicas (2.3), ops of a later build (4.4), stale unredacted copies of
// ops that may arrive after their forget (5.7), forged ops under a replica's own origin past a
// gap, and a replica that loses its file and gets its own ops back (14).
type world struct {
	t     testing.TB
	rng   *rand.Rand
	reps  []*replica
	known map[*replica][]string // record ids each replica has seen
	// seen keeps each op's bytes as written, before a forget redacted them.
	seen     map[OpID][]byte
	seenList []OpID
	targets  []OpID // ops a forget on the link named
	newerSeq uint64 // the seq of the next op from the later build's origin
}

// newerOrigin writes the ops of a later build.
const newerOrigin = "SHA256:newer"

var vocab = []string{"ui", "holder", "sync", "go", "swift", "docs"}

func newWorld(t testing.TB, rng *rand.Rand) *world {
	w := &world{t: t, rng: rng, known: map[*replica][]string{}, seen: map[OpID][]byte{}}
	n := 3 + rng.Intn(3)
	for i := range n {
		r := newReplica(t, fmt.Sprintf("R%d", i))
		r.clock.Set(base.Add(time.Duration(rng.Intn(60000)) * time.Millisecond))
		if rng.Intn(4) == 0 {
			r.clock.offset = time.Duration(rng.Intn(48)-24) * time.Hour // P8's skew, cheaply
		}
		w.reps = append(w.reps, r)
	}
	return w
}

// play makes n random steps: mostly writes on a random replica or an exchange between two, and
// now and then one of the peer cases world lists.
func (w *world) play(n int) {
	for range n {
		r := w.reps[w.rng.Intn(len(w.reps))]
		switch x := w.rng.Intn(100); {
		case x < 25:
			if b := w.reps[w.rng.Intn(len(w.reps))]; b != r {
				w.exchange(r, b)
			}
		case x < 29:
			w.replay(r)
		case x < 32:
			w.newer(r)
		case x < 34:
			w.forgeOwn(r)
		case x < 35:
			w.restore(r)
		case x < 39:
			w.decideApart()
		default:
			w.write(r)
		}
	}
}

// replay sends a replica the first copies of an origin's ops, from the end of
// its gap-free run up to one op, as a stale peer that never saw the op's forget would: unredacted
// bytes that may land after their forget. Most often the op is a forget's target.
func (w *world) replay(r *replica) {
	if len(w.seenList) == 0 {
		return
	}
	id := w.seenList[w.rng.Intn(len(w.seenList))]
	if len(w.targets) > 0 && w.rng.Intn(4) != 0 {
		if t := w.targets[w.rng.Intn(len(w.targets))]; w.seen[t] != nil {
			id = t
		}
	}
	v, err := r.Vector(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	var page [][]byte
	for seq := v[id.Origin] + 1; seq <= id.Seq; seq++ {
		raw, ok := w.seen[OpID{Origin: id.Origin, Seq: seq}]
		if !ok {
			page = nil
			break
		}
		page = append(page, raw)
	}
	if len(page) == 0 {
		page = [][]byte{w.seen[id]}
	}
	if _, err := r.Apply(context.Background(), page); err != nil {
		w.t.Fatal(err)
	}
	w.learn(r)
}

// newer hands a replica one op of a later build: a later version, an unknown kind, an unknown
// field or an unknown record type. It is stored and forwarded, never folded.
func (w *world) newer(r *replica) {
	ctx := context.Background()
	w.newerSeq++
	seq := w.newerSeq
	m := map[string]any{"v": 1, "o": newerOrigin, "s": seq, "w": r.clock.Now().UnixMilli(), "c": 0, "bl": seq}
	root := OpID{Origin: newerOrigin, Seq: seq}.String()
	id := w.pick(r)
	if id != "" {
		if rec, err := r.Get(ctx, id); err == nil {
			root = rec.Root.String()
		}
	}
	switch w.rng.Intn(4) {
	case 0:
		m["v"], m["k"], m["r"], m["root"], m["f"], m["val"] = 2, "set", id+"x", root, "status", map[string]any{"later": true}
	case 1:
		m["k"], m["r"], m["root"], m["val"] = "move", id+"x", root, "elsewhere"
	case 2:
		m["k"], m["r"], m["root"], m["f"], m["val"] = "set", id+"x", root, "estimate", 3
	default:
		m["k"], m["r"], m["root"] = "create", fmt.Sprintf("habit-n%07d", seq), OpID{Origin: newerOrigin, Seq: seq}.String()
		m["val"] = map[string]any{"type": "habit", "fields": map[string]any{"title": "H"}}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		w.t.Fatal(err)
	}
	res, err := r.Apply(ctx, [][]byte{raw})
	if err != nil || res.Stored != 1 {
		w.t.Fatalf("a later build's op: %+v %v, want stored", res, err)
	}
}

// forgeOwn sends a replica an op under its own origin, past a gap: it is refused, and the
// replica's next write still folds.
func (w *world) forgeOwn(r *replica) {
	ctx := context.Background()
	v, err := r.Vector(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	seq := v[r.Origin()] + 2 + uint64(w.rng.Intn(50))
	raw, _ := json.Marshal(map[string]any{"v": 1, "o": r.Origin(), "s": seq, "w": r.clock.Now().UnixMilli(),
		"c": 0, "bl": seq, "k": "create", "r": "task-forged00", "root": OpID{Origin: r.Origin(), Seq: seq}.String(),
		"val": map[string]any{"type": "task", "fields": map[string]any{"title": "forged"}}})
	res, err := r.Apply(ctx, [][]byte{raw})
	if err != nil || res.Refused != 1 {
		w.t.Fatalf("%s: own op past a gap: %+v %v, want refused", r.name, res, err)
	}
	id := NewID(TypeTask)
	if _, err := r.Write(ctx, Batch{Steps: []Step{create(id, TypeTask, map[string]any{"title": "after a forgery"})}}); err != nil {
		w.t.Fatalf("%s: write after a forgery: %v", r.name, err)
	}
	if _, err := r.Get(ctx, id); err != nil {
		w.t.Fatalf("%s: write after a forgery not folded: %v", r.name, err)
	}
	w.learn(r)
}

// restore loses a replica's file once every replica holds every op, then gets its ops back from
// the others: a new file of the same origin. Its next writes must not reuse a seq.
func (w *world) restore(r *replica) {
	w.heal()
	i := slices.Index(w.reps, r)
	n := openReplica(w.t, r.name, filepath.Join(w.t.TempDir(), "logbook.db"))
	n.clock = r.clock
	_ = r.Close()
	w.reps[i] = n
	delete(w.known, r)
	for _, p := range w.reps {
		if p != n {
			pushPages(w.t, p, n, 1+w.rng.Intn(50))
		}
	}
	w.learn(n)
}

// exchange sends some of what from holds that to lacks, in small pages, dropping some ops.
func (w *world) exchange(from, to *replica) {
	ctx := context.Background()
	have, err := to.Vector(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	ops, _, err := from.OpsAfter(ctx, have, 1+w.rng.Intn(12), 0)
	if err != nil {
		w.t.Fatal(err)
	}
	var sent [][]byte
	for _, o := range ops {
		w.see(o)
		if w.rng.Intn(5) != 0 {
			sent = append(sent, o)
		}
	}
	if _, err := to.Apply(ctx, sent); err != nil {
		w.t.Fatal(err)
	}
	w.learn(to)
}

func (w *world) learn(r *replica) {
	list, err := r.List(context.Background(), Filter{Deleted: true})
	if err != nil {
		w.t.Fatal(err)
	}
	ids := w.known[r][:0]
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	w.known[r] = ids
}

// heal exchanges until every replica holds every op.
func (w *world) heal() {
	ctx := context.Background()
	for range 50 {
		for _, a := range w.reps {
			for _, b := range w.reps {
				if a != b {
					pushPages(w.t, a, b, 500)
				}
			}
		}
		v0, _ := w.reps[0].Vector(ctx)
		same := true
		for _, r := range w.reps[1:] {
			v, _ := r.Vector(ctx)
			same = same && fmt.Sprint(v) == fmt.Sprint(v0)
		}
		if same {
			return
		}
	}
	w.t.Fatal("replicas did not converge")
}

// close closes the replicas' files; a long property run would run out of descriptors.
func (w *world) close() {
	for _, r := range w.reps {
		_ = r.Close()
	}
}

// project returns a project id: one the replica holds, else one it may never hold.
func (w *world) project(r *replica) string {
	var ps []string
	for _, id := range w.known[r] {
		if strings.HasPrefix(id, "proj-") {
			ps = append(ps, id)
		}
	}
	if len(ps) == 0 || w.rng.Intn(4) == 0 {
		return fmt.Sprintf("proj-faraway%d", w.rng.Intn(3))
	}
	return ps[w.rng.Intn(len(ps))]
}

func (w *world) pick(r *replica) string {
	ids := w.known[r]
	if len(ids) == 0 {
		return ""
	}
	return ids[w.rng.Intn(len(ids))]
}

// write tries one random batch; a refused one is fine.
func (w *world) write(r *replica) {
	w.writeSteps(r, w.steps(r))
}

// approve is an approve batch of proposal id (9.2): the draft's record, linked from the
// proposal, a change to another record, and the decision. writeSteps gives it an approve's actor.
// A third of the time the draft's record is itself a proposal, whose fold waits for this one's
// standing decision whatever the two roots' order.
func (w *world) approve(r *replica, id, status string) []Step {
	typ, fields := TypeTask, map[string]any{"title": "made"}
	if w.rng.Intn(3) == 0 {
		typ, fields["target"] = TypeProposal, "record"
	}
	made := NewID(typ)
	st := []Step{create(made, typ, fields), add(made, "links", Link{Kind: LinkFrom, To: id})}
	if other := w.pick(r); other != "" && other != id {
		if rec, err := r.Get(context.Background(), other); err == nil && !rec.Deleted {
			st = append(st, note(other, "from an approve"))
		}
	}
	return append(st, set(id, "status", status), set(id, "decided_by", r.name))
}

// decideApart has two replicas decide one proposal both hold pending, with no link between the
// writes (9.3): one approves with a batch that makes a record, the other rejects or approves too.
// One decision is overruled once they link. With no such proposal, a new one is filed on the
// first and synced whole to the second first.
func (w *world) decideApart() {
	rng := w.rng
	a, b := w.reps[rng.Intn(len(w.reps))], w.reps[rng.Intn(len(w.reps))]
	if a == b {
		return
	}
	pending := func(r *replica, id string) bool {
		rec, err := r.Get(context.Background(), id)
		return err == nil && rec.Type == TypeProposal && rec.Status == "pending" && !rec.Deleted
	}
	ids := w.known[a]
	for _, k := range rng.Perm(len(ids)) {
		id := ids[k]
		if !pending(a, id) || !pending(b, id) {
			continue
		}
		w.writeSteps(a, w.approve(a, id, approvals[rng.Intn(len(approvals))]))
		if rng.Intn(2) == 0 {
			w.writeSteps(b, w.approve(b, id, approvals[rng.Intn(len(approvals))]))
		} else {
			w.writeSteps(b, []Step{set(id, "status", "rejected"), set(id, "decided_by", b.name)})
		}
		return
	}
	// The replica further along its own seqs files, the other approves: the approve's records
	// then mostly get roots that sort before the proposal's ("3@R1" before "7@R0"), the order a
	// fold must not depend on.
	own := func(r *replica) uint64 {
		v, err := r.Vector(context.Background())
		if err != nil {
			w.t.Fatal(err)
		}
		return v[r.Origin()]
	}
	if own(a) < own(b) {
		a, b = b, a
	}
	x := NewID(TypeProposal)
	w.writeSteps(a, []Step{create(x, TypeProposal, map[string]any{"title": "apart", "target": "record"})})
	push(w.t, a, b)
	w.learn(b)
	if pending(a, x) && pending(b, x) {
		w.writeSteps(b, w.approve(b, x, approvals[rng.Intn(len(approvals))]))
		w.writeSteps(a, []Step{set(x, "status", "rejected"), set(x, "decided_by", a.name)})
	}
}

// writeSteps tries one batch on r; a refused one is fine.
func (w *world) writeSteps(r *replica, steps []Step) {
	ctx := context.Background()
	rng := w.rng
	r.clock.Advance(time.Duration(rng.Intn(3000)) * time.Millisecond)
	if len(steps) == 0 {
		return
	}
	actor := &Actor{Kind: "agent", Harness: "claude", Session: fmt.Sprintf("s%d", rng.Intn(3))}
	if steps[0].Kind == KindCreate && slices.ContainsFunc(steps, func(st Step) bool { return st.Field == "decided_by" }) {
		actor.ApprovedBy = "SHA256:device-" + r.name
	}
	res, err := r.Write(ctx, Batch{Actor: actor, Steps: steps})
	if err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
		w.t.Fatalf("%s write %+v: %v", r.name, steps, err)
	}
	for _, id := range res.Ops {
		if err != nil {
			break
		}
		var raw []byte
		if err := r.db.QueryRowContext(ctx, `SELECT raw FROM ops WHERE origin = ? AND seq = ?`, id.Origin, id.Seq).Scan(&raw); err != nil {
			w.t.Fatal(err)
		}
		w.see(raw)
	}
	w.learn(r)
}

// see keeps an op's bytes as first written or carried, before any forget redacted them.
func (w *world) see(raw []byte) {
	op, err := decodeOp(raw)
	if err != nil {
		return
	}
	if _, ok := w.seen[op.ID()]; !ok {
		w.seen[op.ID()] = raw
		w.seenList = append(w.seenList, op.ID())
	}
	if op.Kind == KindForget && op.Target != nil {
		w.targets = append(w.targets, *op.Target)
	}
}

func (w *world) steps(r *replica) []Step {
	rng := w.rng
	id := w.pick(r)
	if id == "" || rng.Intn(5) == 0 {
		typ := Types[rng.Intn(len(Types))]
		nid := NewID(typ)
		if rng.Intn(3) == 0 {
			// A small pool of ids for every type, so replicas create one id apart (2.3).
			nid = fmt.Sprintf("rec-pool%04d", rng.Intn(2))
		}
		fields := map[string]any{"title": fmt.Sprintf("%s %d", typ, rng.Intn(1000))}
		if typ != TypeProject && rng.Intn(3) == 0 {
			fields["project"] = w.project(r)
		}
		if rng.Intn(2) == 0 {
			fields["body"] = vocab[rng.Intn(len(vocab))] + " body"
		}
		if typ == TypeProposal {
			fields["target"] = "record"
		}
		steps := []Step{create(nid, typ, fields)}
		if rng.Intn(2) == 0 {
			steps = append(steps, add(nid, "tags", vocab[rng.Intn(len(vocab))]))
		}
		if old := w.pick(r); old != "" && rng.Intn(2) == 0 {
			kind := LinkRelates
			if typ == TypeFinding {
				kind = LinkSupersedes
			}
			steps = append(steps, add(nid, "links", Link{Kind: kind, To: old}))
		}
		if typ == TypeProject {
			steps = append(steps, add(nid, "identities", fmt.Sprintf("forge.example/o/r%d", rng.Intn(4))))
		}
		return steps
	}
	rec, err := r.Get(context.Background(), id)
	if err != nil {
		return nil
	}
	switch rng.Intn(10) {
	case 9:
		return []Step{set(id, "project", w.project(r))}
	case 0:
		return []Step{set(id, "title", fmt.Sprintf("title %d", rng.Intn(1000)))}
	case 1:
		return []Step{set(id, "body", vocab[rng.Intn(len(vocab))]+" text")}
	case 2:
		if list := statuses[rec.Type]; len(list) > 0 {
			status := list[rng.Intn(len(list))]
			if rec.Type == TypeProposal && slices.Contains(approvals, status) && rng.Intn(2) == 0 {
				return w.approve(r, id, status)
			}
			st := []Step{set(id, "status", status)}
			if rec.Type == TypeProposal {
				st = append(st, set(id, "decided_by", r.name))
			}
			return st
		}
	case 3:
		return []Step{add(id, "tags", vocab[rng.Intn(len(vocab))])}
	case 4:
		if len(rec.Tags) > 0 {
			return []Step{remove(id, "tags", rec.Tags[rng.Intn(len(rec.Tags))])}
		}
	case 5:
		return []Step{note(id, fmt.Sprintf("note %d %s", rng.Intn(1000), vocab[rng.Intn(len(vocab))]))}
	case 6:
		return []Step{set(id, "deleted", rng.Intn(3) != 0 && !rec.Deleted)}
	case 7:
		switch rec.Type {
		case TypeTask:
			return []Step{set(id, "type", TypeIssue), set(id, "priority", rng.Intn(4))}
		case TypeIssue:
			return []Step{set(id, "type", TypeTask), set(id, "assignee", "s1")}
		case TypeGuide:
			return []Step{set(id, "pinned", rng.Intn(2) == 0)}
		}
	case 8:
		ops, err := r.History(context.Background(), id)
		if err != nil {
			break
		}
		// Mostly an op that holds text; now and then any op, which a write refuses.
		text := slices.DeleteFunc(slices.Clone(ops), func(o *Op) bool {
			return !(o.Kind == KindAppend || o.Kind == KindAdd || o.Kind == KindCreate ||
				(o.Kind == KindSet && (o.Field == "title" || o.Field == "body")))
		})
		if len(text) > 0 && rng.Intn(4) != 0 {
			ops = text
		}
		if len(ops) > 0 {
			return []Step{{Kind: KindForget, Target: ops[rng.Intn(len(ops))].ID()}}
		}
	}
	return []Step{note(id, "fallback")}
}

// P2. Fold equals rebuild: on every replica, the tables folded op by op as ops arrived equal the
// tables Rebuild makes from the log. After the heal, every replica also folds the same tables.
func TestP2FoldEqualsRebuild(t *testing.T) {
	t.Parallel()
	sd := seed(t)
	n := runs(100, 20000)
	if testing.Short() {
		n = 20
	}
	eachRun(t, n, func(t *testing.T, i int) {
		rng := rand.New(rand.NewSource(sd + int64(i)))
		w := newWorld(t, rng)
		w.play(15 + rng.Intn(25))
		checkRebuild(t, w, sd, i)
		w.heal()
		ref, refOps := dump(t, w.reps[0].Store), dumpOps(t, w.reps[0].Store)
		for _, r := range w.reps[1:] {
			if d := dump(t, r.Store); d != ref {
				t.Fatalf("seed %d run %d: %s folds differently from %s after heal:\n%s\n---\n%s", sd, i, r.name, w.reps[0].name, d, ref)
			}
			if d := dumpOps(t, r.Store); d != refOps {
				t.Fatalf("seed %d run %d: %s holds other op bytes than %s after heal:\n%s\n---\n%s", sd, i, r.name, w.reps[0].name, d, refOps)
			}
		}
		checkRebuild(t, w, sd, i)
		w.close()
	})
}

// eachRun runs a property's runs as parallel subtests named run<i>, up to -parallel at a time:
// a run spends most of its time waiting on SQLite's file setup, not on the CPU. Run i uses seed
// plus i, so `-run 'TestP2FoldEqualsRebuild/run17$'` with $GFF_LOGBOOK_SEED replays one.
func eachRun(t *testing.T, n int, run func(t *testing.T, i int)) {
	for i := range n {
		t.Run(fmt.Sprintf("run%d", i), func(t *testing.T) {
			t.Parallel()
			run(t, i)
		})
	}
}

func checkRebuild(t *testing.T, w *world, sd int64, run int) {
	t.Helper()
	for _, r := range w.reps {
		before := dump(t, r.Store)
		if err := r.Rebuild(context.Background()); err != nil {
			t.Fatal(err)
		}
		if after := dump(t, r.Store); after != before {
			t.Fatalf("seed %d run %d: %s: fold and rebuild differ:\nfold:\n%s\nrebuild:\n%s", sd, run, r.name, before, after)
		}
	}
}

// P3. Order does not matter: the same ops, delivered in any order in any page sizes, fold to the
// same records. Half the runs keep each origin's seq order, as sync does; half shuffle freely,
// which the gap rule must absorb.
func TestP3OrderDoesNotMatter(t *testing.T) {
	t.Parallel()
	sd := seed(t)
	n := runs(50, 5000)
	if testing.Short() {
		n = 10
	}
	ctx := context.Background()
	eachRun(t, n, func(t *testing.T, i int) {
		rng := rand.New(rand.NewSource(sd + int64(i)))
		w := newWorld(t, rng)
		w.play(15 + rng.Intn(25))
		w.heal()
		all, _, err := w.reps[0].OpsAfter(ctx, Vector{}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		ref := dump(t, w.reps[0].Store)
		for k := range 3 {
			order := shuffled(rng, all, k%2 == 0)
			obs := newReplica(t, fmt.Sprintf("obs%d", k))
			for len(order) > 0 {
				m := 1 + rng.Intn(min(8, len(order)))
				if _, err := obs.Apply(ctx, order[:m]); err != nil {
					t.Fatal(err)
				}
				order = order[m:]
			}
			if d := dump(t, obs.Store); d != ref {
				t.Fatalf("seed %d run %d order %d: folds differently:\n%s\n---\n%s", sd, i, k, d, ref)
			}
			_ = obs.Close()
		}
		w.close()
	})
}

// shuffled returns ops in a random order: an interleaving of the per-origin streams when keepSeq,
// else any order.
func shuffled(rng *rand.Rand, ops [][]byte, keepSeq bool) [][]byte {
	out := slices.Clone(ops)
	if !keepSeq {
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	streams := map[string][][]byte{}
	var origins []string
	for _, raw := range ops {
		o, _ := decodeOp(raw)
		if _, ok := streams[o.Origin]; !ok {
			origins = append(origins, o.Origin)
		}
		streams[o.Origin] = append(streams[o.Origin], raw)
	}
	out = out[:0]
	for len(origins) > 0 {
		i := rng.Intn(len(origins))
		s := streams[origins[i]]
		out = append(out, s[0])
		streams[origins[i]] = s[1:]
		if len(s) == 1 {
			origins = slices.Delete(origins, i, i+1)
		}
	}
	return out
}

// P6. Conflicts are exact: a field is flagged if and only if two set ops on it are concurrent by
// prev with neither seen by a later op, that is, the field has two or more heads; the flag lists
// exactly the heads; the highest stamp wins. Histories are generated with known causality and
// delivered in random order.
func TestP6ConflictsAreExact(t *testing.T) {
	t.Parallel()
	sd := seed(t)
	rng := rand.New(rand.NewSource(sd))
	n := runs(500, 20000)
	if testing.Short() {
		n = 50
	}
	ctx := context.Background()
	r := newReplica(t, "observer")
	origins := []string{"SHA256:o1", "SHA256:o2", "SHA256:o3"}
	seqs := map[string]uint64{}
	walls := map[string]int64{}
	for run := range n {
		type gen struct {
			op   map[string]any
			id   OpID
			wall int64
			prev []int
		}
		var all [][]byte
		mk := func(origin string, wall int64, m map[string]any) OpID {
			seqs[origin]++
			if wall <= walls[origin] {
				wall = walls[origin] + 1
			}
			walls[origin] = wall
			id := OpID{Origin: origin, Seq: seqs[origin]}
			m["v"], m["o"], m["s"], m["w"], m["c"], m["bl"] = 1, origin, id.Seq, wall, 0, id.Seq
			all = append(all, rawOp(t, m))
			return id
		}
		rid := fmt.Sprintf("task-p6r%05d", run)
		cOrigin := origins[rng.Intn(3)]
		cWall := base.UnixMilli() + int64(run)*1_000_000
		root := OpID{Origin: cOrigin, Seq: seqs[cOrigin] + 1}
		mk(cOrigin, cWall, map[string]any{"k": "create", "r": rid, "root": root.String(),
			"val": map[string]any{"type": "task", "fields": map[string]any{"title": "t0", "status": "open"}}})
		cWall = walls[cOrigin]

		type fieldWant struct {
			ids   []OpID
			walls []int64
			seen  []map[int]bool
			value []string
		}
		want := map[string]*fieldWant{}
		for _, field := range []string{"title", "status"} {
			fw := &fieldWant{ids: []OpID{root}, walls: []int64{cWall}, seen: []map[int]bool{{}}, value: []string{map[string]string{"title": "t0", "status": "open"}[field]}}
			want[field] = fw
			k := 1 + rng.Intn(7)
			for j := range k {
				// Prev: a random subset of the earlier ops on this field; usually a small one.
				var prev []int
				for p := range fw.ids {
					if rng.Intn(len(fw.ids)+1) == 0 || (len(prev) == 0 && p == len(fw.ids)-1 && rng.Intn(3) != 0) {
						prev = append(prev, p)
					}
				}
				seen := map[int]bool{}
				maxWall := int64(0)
				var prevIDs []string
				for _, p := range prev {
					seen[p] = true
					for q := range fw.seen[p] {
						seen[q] = true
					}
					maxWall = max(maxWall, fw.walls[p])
					prevIDs = append(prevIDs, fw.ids[p].String())
				}
				origin := origins[rng.Intn(3)]
				wall := max(maxWall, cWall) + 1 + int64(rng.Intn(5000))
				val := fmt.Sprintf("%s-%d", field, j)
				if field == "status" {
					val = statuses[TypeTask][rng.Intn(6)]
				}
				m := map[string]any{"k": "set", "r": rid, "root": root.String(), "f": field, "val": val}
				if len(prevIDs) > 0 {
					m["prev"] = prevIDs
				}
				id := mk(origin, wall, m)
				fw.ids = append(fw.ids, id)
				fw.walls = append(fw.walls, walls[origin])
				fw.seen = append(fw.seen, seen)
				fw.value = append(fw.value, val)
			}
		}
		rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		if _, err := r.Apply(ctx, all); err != nil {
			t.Fatal(err)
		}
		rec, err := r.Get(ctx, rid)
		if err != nil {
			t.Fatalf("seed %d run %d: %v", sd, run, err)
		}
		for field, fw := range want {
			var heads []OpID
			best := 0
			for i := range fw.ids {
				inSeen := false
				for j := range fw.ids {
					if fw.seen[j][i] {
						inSeen = true
					}
				}
				if !inSeen {
					heads = append(heads, fw.ids[i])
				}
				if fw.walls[i] > fw.walls[best] || (fw.walls[i] == fw.walls[best] && fw.ids[i].Origin > fw.ids[best].Origin) {
					best = i
				}
			}
			var got []OpID
			for _, f := range rec.Flags {
				if f.Kind == FlagConflict && f.Field == field {
					for _, v := range f.Values {
						got = append(got, v.Op)
					}
				}
			}
			sortIDs := func(s []OpID) { slices.SortFunc(s, func(a, b OpID) int { return compareStr(a.String(), b.String()) }) }
			sortIDs(heads)
			sortIDs(got)
			if len(heads) < 2 {
				heads = nil
			}
			if !slices.Equal(got, heads) {
				t.Fatalf("seed %d run %d %s: flagged %v, want %v", sd, run, field, got, heads)
			}
			if fv := rec.Fields[field]; fv.Op != fw.ids[best] || str(fv.Value) != fw.value[best] {
				t.Fatalf("seed %d run %d %s: winner %v %s, want %v %s", sd, run, field, fv.Op, fv.Value, fw.ids[best], fw.value[best])
			}
		}
	}
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
