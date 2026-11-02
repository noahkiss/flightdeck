package logbook

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Property tests of logbook.md 17 that stage 3 owns: P1, P4, P5, P7, P8 and P9. Replicas are
// real stores on temp files with fake clocks, in one process. They sync with Store.Sync, the
// exchange gff-hab runs over a peer link, through a fake link that drops, delays, repeats and
// reorders what it carries. Each run logs its seed; $GFF_LOGBOOK_SEED replays one.

// simOpts picks the faults and the topology of one property's runs.
type simOpts struct {
	line       bool    // three replicas A-B-C; A and C never link (P7)
	middleDown bool    // B is down most of the time (P7)
	drop       float64 // a request lost before it arrives
	delay      float64 // a request held and delivered later, after others: a reorder
	repeat     float64 // a request delivered twice, now or later
	lostAnswer float64 // a request served whose answer is lost
	crashes    bool    // replicas stop and start again on the same file
	wipes      bool    // a replica's disk dies; it comes back with a new key and an empty file (P4)
	skew       bool    // clocks a day off with drift; a write after a sync orders after all it saw (P8)
	secrets    bool    // secrets noted and forgotten during a partition (P9)
	steps      int
}

var (
	errLinkDown = errors.New("link down")
	errDropped  = errors.New("dropped")
	errDelayed  = errors.New("timed out (delayed)")
	errLost     = errors.New("answer lost")
)

// sim is a world of replicas joined by links that fail.
type sim struct {
	*world
	o       simOpts
	links   [][2]int // dialer, dialed
	cut     map[[2]int]bool
	down    map[int]bool
	epoch   []int // moves when a replica restarts or is rebuilt: in-flight requests to or from it die
	delayed []func()
	drift   []float64
	// written is the ledger of every op a replica wrote; lost holds those of a wiped replica
	// that no other replica held when its disk died. ownSeen is each replica's own run read last.
	written  map[OpID]bool
	lost     map[OpID]bool
	ownSeen  []uint64
	secrets  []string
	gen      int // names rebuilt replicas
	repeated int // repeats delivered, for the log
	// page is what each link asks for per page: in half the runs 3 ops or 2 KiB, so exchanges
	// page through More, pass the byte bound and cut origins mid-run.
	page Limits
}

func newSim(t *testing.T, rng *rand.Rand, o simOpts) *sim {
	w := newWorld(t, rng)
	if o.line {
		for _, r := range w.reps[3:] {
			_ = r.Close()
		}
		w.reps = w.reps[:3]
	}
	s := &sim{world: w, o: o, cut: map[[2]int]bool{}, down: map[int]bool{},
		written: map[OpID]bool{}, lost: map[OpID]bool{}}
	if rng.Intn(2) == 0 {
		s.page = Limits{Ops: 3, Bytes: 2 << 10}
	}
	n := len(w.reps)
	s.epoch = make([]int, n)
	s.ownSeen = make([]uint64, n)
	s.drift = make([]float64, n)
	for i, r := range w.reps {
		r.clock.offset = 0
		s.drift[i] = 1
		if o.skew {
			r.clock.offset = time.Duration(rng.Int63n(int64(48*time.Hour))) - 24*time.Hour
			s.drift[i] = 0.5 + rng.Float64()*1.5
		}
	}
	if o.line {
		// A and C never link. Either both dial B, or B dials both.
		if rng.Intn(2) == 0 {
			s.links = [][2]int{{0, 1}, {2, 1}}
		} else {
			s.links = [][2]int{{1, 0}, {1, 2}}
		}
	} else {
		for i := range n {
			for j := i + 1; j < n; j++ {
				if rng.Intn(2) == 0 {
					s.links = append(s.links, [2]int{i, j})
				} else {
					s.links = append(s.links, [2]int{j, i})
				}
			}
		}
	}
	return s
}

// remote is the dialed end of a link as the dialer sees it.
type remote struct {
	s        *sim
	from, to int
	faulty   bool
}

func (r remote) Pull(ctx context.Context, have Vector, skip []string, lim Limits) (PullAnswer, error) {
	var a PullAnswer
	err := r.s.carry(r.from, r.to, r.faulty, func(to *replica) error {
		var err error
		a, err = to.ServePull(ctx, have, skip, lim)
		return err
	})
	return a, err
}

func (r remote) Page() Limits { return r.s.page }

func (r remote) Push(ctx context.Context, ops [][]byte) (PushAnswer, error) {
	var a PushAnswer
	err := r.s.carry(r.from, r.to, r.faulty, func(to *replica) error {
		var err error
		a, _, err = to.ServePush(ctx, ops)
		return err
	})
	return a, err
}

func (s *sim) connected(a, b int) bool {
	return !s.down[a] && !s.down[b] && !s.cut[[2]int{a, b}] && !s.cut[[2]int{b, a}]
}

// carry takes one request from replica from to replica to over the link. serve runs it on the
// replica at to's index when it arrives; a request held for later dies when either end restarts.
func (s *sim) carry(from, to int, faulty bool, serve func(*replica) error) error {
	if !s.connected(from, to) {
		return errLinkDown
	}
	run := func() {
		if err := serve(s.reps[to]); err != nil {
			s.t.Fatalf("serve on %s: %v", s.reps[to].name, err)
		}
	}
	later := func() {
		ef, et := s.epoch[from], s.epoch[to]
		s.delayed = append(s.delayed, func() {
			if s.epoch[from] == ef && s.epoch[to] == et {
				run()
			}
		})
	}
	if !faulty {
		run()
		return nil
	}
	o := s.o
	x := s.rng.Float64()
	switch {
	case x < o.drop:
		return errDropped
	case x < o.drop+o.delay:
		later()
		return errDelayed
	case x < o.drop+o.delay+o.repeat:
		run()
		s.repeated++
		if s.rng.Intn(2) == 0 {
			// At once: P5 checks the second delivery changes nothing.
			before, beforeOps := dump(s.t, s.reps[to].Store), dumpOps(s.t, s.reps[to].Store)
			run()
			if dump(s.t, s.reps[to].Store) != before || dumpOps(s.t, s.reps[to].Store) != beforeOps {
				s.t.Fatalf("a repeated request changed %s", s.reps[to].name)
			}
		} else {
			later()
		}
		return nil
	case x < o.drop+o.delay+o.repeat+o.lostAnswer:
		run()
		return errLost
	}
	run()
	return nil
}

// ledger adds a replica's own new ops to the written ledger.
func (s *sim) ledger(i int) {
	r := s.reps[i]
	v, err := r.Vector(context.Background())
	if err != nil {
		s.t.Fatal(err)
	}
	for seq := s.ownSeen[i] + 1; seq <= v[r.Origin()]; seq++ {
		s.written[OpID{Origin: r.Origin(), Seq: seq}] = true
	}
	s.ownSeen[i] = max(s.ownSeen[i], v[r.Origin()])
}

// sync runs one exchange over link l, through the faulty link.
func (s *sim) sync(l [2]int, faulty bool) (SyncResult, error) {
	res, err := s.reps[l[0]].Sync(context.Background(), remote{s: s, from: l[0], to: l[1], faulty: faulty})
	if err != nil && !errors.Is(err, errLinkDown) && !errors.Is(err, errDropped) && !errors.Is(err, errDelayed) && !errors.Is(err, errLost) {
		s.t.Fatalf("sync %s -> %s: %v", s.reps[l[0]].name, s.reps[l[1]].name, err)
	}
	s.learn(s.reps[l[0]])
	return res, err
}

func (s *sim) play() {
	o := s.o
	for range o.steps {
		i := s.rng.Intn(len(s.reps))
		switch x := s.rng.Intn(100); {
		case x < 40:
			s.world.write(s.reps[i])
			s.ledger(i)
		case x < 70:
			l := s.links[s.rng.Intn(len(s.links))]
			res, err := s.sync(l, true)
			if o.skew && err == nil && res.Pulled.Stored > 0 {
				s.ordersAfter(l[0])
			}
		case x < 77:
			if len(s.delayed) > 0 {
				k := s.rng.Intn(len(s.delayed))
				f := s.delayed[k]
				s.delayed = slices.Delete(s.delayed, k, k+1)
				f()
			}
		case x < 85:
			s.partition()
		case x < 87:
			if o.crashes {
				s.restart(i)
			}
		case x < 88:
			if o.wipes && i != 0 {
				s.wipe(i)
			}
		case x < 90:
			if !o.wipes || i == 0 {
				s.world.newer(s.reps[0]) // only replica 0, which is never wiped: no permanent gap
			}
		case x < 94:
			if o.secrets {
				s.secret(i)
			}
		default:
			for j, r := range s.reps {
				r.clock.Advance(time.Duration(float64(s.rng.Intn(5000))*s.drift[j]) * time.Millisecond)
			}
		}
	}
}

// partition cuts or mends a link, or takes a replica down or up. In a line B goes down often.
func (s *sim) partition() {
	if s.o.middleDown && s.rng.Intn(3) != 0 {
		s.down[1] = s.rng.Intn(4) != 0
		return
	}
	if s.rng.Intn(3) == 0 {
		i := s.rng.Intn(len(s.reps))
		s.down[i] = !s.down[i]
		return
	}
	l := s.links[s.rng.Intn(len(s.links))]
	s.cut[l] = !s.cut[l]
}

func (s *sim) partitioned() bool {
	for _, d := range s.down {
		if d {
			return true
		}
	}
	for _, c := range s.cut {
		if c {
			return true
		}
	}
	return false
}

// restart closes a replica and opens its file again: requests in flight to or from it die.
func (s *sim) restart(i int) {
	r := s.reps[i]
	_ = r.Close()
	n := openReplica(s.t, r.name, r.path)
	n.clock = r.clock
	s.reps[i] = n
	s.known[n] = s.known[r]
	delete(s.known, r)
	s.epoch[i]++
}

// wipe loses a replica's disk: it comes back with a new key (a new origin) and an empty file
// (7.4). The ops no other replica held are lost; P4 excuses only those.
func (s *sim) wipe(i int) {
	ctx := context.Background()
	r := s.reps[i]
	s.ledger(i)
	// Any op only this disk held is lost: its own, and others' it alone still carried.
	for id := range s.written {
		if s.lost[id] {
			continue
		}
		held := false
		for j, p := range s.reps {
			if j != i && hasOp(s.t, p.Store, id) {
				held = true
			}
		}
		if !held {
			s.lost[id] = true
		}
	}
	_ = r.Close()
	s.gen++
	n := openReplica(s.t, fmt.Sprintf("%s.%d", r.name, s.gen), filepath.Join(s.t.TempDir(), "logbook.db"))
	n.clock = r.clock
	s.reps[i] = n
	delete(s.known, r)
	s.epoch[i]++
	s.ownSeen[i] = 0
	if v, _ := n.Vector(ctx); len(v) != 0 {
		s.t.Fatalf("a rebuilt replica starts with %v", v)
	}
}

// secret notes a secret on a replica while the replicas are split, syncs a little so it may
// spread, and forgets it from a replica that holds it.
func (s *sim) secret(i int) {
	if !s.partitioned() {
		s.partition()
	}
	ctx := context.Background()
	r := s.reps[i]
	id := s.pick(r)
	if id == "" {
		return
	}
	text := fmt.Sprintf("zq%06dsecretqz", s.rng.Intn(1_000_000))
	res, err := r.Write(ctx, Batch{Steps: []Step{note(id, "the key is "+text)}})
	if err != nil {
		return // a deleted or proposal record refuses notes
	}
	s.ledger(i)
	s.secrets = append(s.secrets, text)
	for range s.rng.Intn(3) {
		s.sync(s.links[s.rng.Intn(len(s.links))], true)
	}
	// Forget from a replica that holds the note, on its side of the split.
	var holders []int
	for j, p := range s.reps {
		if hasOp(s.t, p.Store, res.Ops[0]) {
			holders = append(holders, j)
		}
	}
	j := holders[s.rng.Intn(len(holders))]
	if _, err := s.reps[j].Write(ctx, Batch{Steps: []Step{{Kind: KindForget, Target: res.Ops[0]}}}); err != nil {
		s.t.Fatalf("forget the secret on %s: %v", s.reps[j].name, err)
	}
	s.ledger(j)
}

// ordersAfter checks P8's causality: a write made after a sync orders after every op the
// replica holds, whatever its clock says.
func (s *sim) ordersAfter(i int) {
	ctx := context.Background()
	r := s.reps[i]
	var top HLC
	if err := r.db.QueryRowContext(ctx, `SELECT wall, counter, origin FROM ops ORDER BY wall DESC, counter DESC, origin DESC LIMIT 1`).
		Scan(&top.Wall, &top.Counter, &top.Origin); err != nil {
		s.t.Fatal(err)
	}
	id := NewID(TypeTask)
	res, err := r.Write(ctx, Batch{Steps: []Step{create(id, TypeTask, map[string]any{"title": "after a sync"})}})
	if err != nil {
		if errors.Is(err, ErrSharedOrigin) {
			return
		}
		s.t.Fatalf("%s: write after a sync: %v", r.name, err)
	}
	s.ledger(i)
	s.learn(r)
	var got HLC
	if err := r.db.QueryRowContext(ctx, `SELECT wall, counter, origin FROM ops WHERE origin = ? AND seq = ?`,
		res.Ops[0].Origin, res.Ops[0].Seq).Scan(&got.Wall, &got.Counter, &got.Origin); err != nil {
		s.t.Fatal(err)
	}
	if got.Compare(top) <= 0 {
		s.t.Fatalf("%s (clock %v): a write after a sync stamped %v, not after %v", r.name, r.clock.Now(), got, top)
	}
}

// heal ends every fault: what is in flight arrives, every link mends, and the links sync with
// no fault until a whole pass moves nothing.
func (s *sim) heal() {
	ctx := context.Background()
	for _, f := range s.delayed {
		f()
	}
	s.delayed = nil
	clear(s.cut)
	clear(s.down)
	for range 20 {
		moved := false
		for _, l := range s.links {
			before, _ := s.reps[l[1]].Vector(ctx)
			res, err := s.sync(l, false)
			if err != nil {
				s.t.Fatalf("sync with no fault: %v", err)
			}
			if res.Stalled != "" {
				s.t.Fatalf("sync %s -> %s stalled on %s", s.reps[l[0]].name, s.reps[l[1]].name, res.Stalled)
			}
			after, _ := s.reps[l[1]].Vector(ctx)
			moved = moved || res.Pulled.Stored > 0 || fmt.Sprint(before) != fmt.Sprint(after)
		}
		if !moved {
			return
		}
	}
	s.t.Fatal("sync did not go quiet")
}

// converged checks P1: every replica holds the same ops and folds the same records and flags.
func (s *sim) converged(sd int64, run int) {
	ref, refOps := dump(s.t, s.reps[0].Store), dumpOps(s.t, s.reps[0].Store)
	for _, r := range s.reps[1:] {
		if d := dump(s.t, r.Store); d != ref {
			s.t.Fatalf("seed %d run %d: %s folds differently from %s after heal:\n%s\n---\n%s", sd, run, r.name, s.reps[0].name, d, ref)
		}
		if d := dumpOps(s.t, r.Store); d != refOps {
			s.t.Fatalf("seed %d run %d: %s holds other ops than %s after heal:\n%s\n---\n%s", sd, run, r.name, s.reps[0].name, d, refOps)
		}
	}
}

// noLostWrite checks P4: every op written is on every replica, except those of a wiped disk
// that no other replica held.
func (s *sim) noLostWrite(sd int64, run int) {
	for i := range s.reps {
		s.ledger(i)
	}
	for id := range s.written {
		if s.lost[id] {
			continue
		}
		for _, r := range s.reps {
			if !hasOp(s.t, r.Store, id) {
				s.t.Fatalf("seed %d run %d: op %v was written and is not on %s", sd, run, id, r.name)
			}
		}
	}
}

// forgotten checks P9: no replica's files, op log or search index hold a forgotten secret.
func (s *sim) forgotten(sd int64, run int) {
	for _, text := range s.secrets {
		for _, r := range s.reps {
			if got, err := r.Find(context.Background(), text, 5); err != nil || len(got) > 0 {
				s.t.Fatalf("seed %d run %d: %s finds the forgotten %q: %v %v", sd, run, r.name, text, got, err)
			}
			var n int
			if err := r.db.QueryRow(`SELECT count(*) FROM ops WHERE instr(raw, ?) > 0`, text).Scan(&n); err != nil || n > 0 {
				s.t.Fatalf("seed %d run %d: %s's op log holds the forgotten %q (%d, %v)", sd, run, r.name, text, n, err)
			}
			if got := fileHolds(s.t.(*testing.T), r.path, text); len(got) > 0 {
				s.t.Fatalf("seed %d run %d: %s's files hold the forgotten %q: %v", sd, run, r.name, text, got)
			}
		}
	}
}

func hasOp(t testing.TB, s *Store, id OpID) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM ops WHERE origin = ? AND seq = ?`, id.Origin, id.Seq).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// runSim runs a property: n seeded runs of play, heal and the checks.
func runSim(t *testing.T, n int, o simOpts, check func(s *sim, sd int64, run int)) {
	t.Parallel()
	sd := seed(t)
	if testing.Short() {
		n = max(1, n/10)
	}
	eachRun(t, n, func(t *testing.T, i int) {
		rng := rand.New(rand.NewSource(sd + int64(i)))
		s := newSim(t, rng, o)
		s.play()
		s.heal()
		s.converged(sd, i)
		if check != nil {
			check(s, sd, i)
		}
		s.close()
	})
}

// P1. Convergence: after every partition heals and sync goes quiet, all replicas hold the same
// ops and fold the same records and conflict flags. The link drops, delays, repeats and loses
// answers; replicas restart.
func TestP1Convergence(t *testing.T) {
	runSim(t, runs(40, 20000), simOpts{drop: 0.1, delay: 0.1, repeat: 0.1, lostAnswer: 0.1, crashes: true, steps: 60},
		func(s *sim, sd int64, run int) {
			if run%10 == 0 {
				checkRebuild(s.t.(*testing.T), s.world, sd, run) // P2 after P1, on a share of the runs
			}
		})
}

// P4. No lost write: every op a replica wrote is on every replica after heal, unless its disk
// died before any other replica held it. Replicas crash, and some are rebuilt with a new key.
func TestP4NoLostWrite(t *testing.T) {
	runSim(t, runs(30, 5000), simOpts{drop: 0.15, delay: 0.1, lostAnswer: 0.1, crashes: true, wipes: true, steps: 60},
		func(s *sim, sd int64, run int) { s.noLostWrite(sd, run) })
}

// P5. Repeats are harmless: the link repeats 10% of requests, at once (checked to change
// nothing) or later, after other requests.
func TestP5RepeatsAreHarmless(t *testing.T) {
	runSim(t, runs(20, 5000), simOpts{repeat: 0.1, steps: 60}, func(s *sim, sd int64, run int) {
		s.noLostWrite(sd, run)
	})
}

// P7. Multi-hop: in a line A-B-C with B down most of the time, A and C, which never link,
// converge through B.
func TestP7MultiHop(t *testing.T) {
	runSim(t, runs(20, 5000), simOpts{line: true, middleDown: true, drop: 0.1, delay: 0.05, steps: 60},
		func(s *sim, sd int64, run int) { s.noLostWrite(sd, run) })
}

// P8. Clock skew: clocks up to a day off, each drifting at its own rate, still converge, and a
// write made after a sync orders after every op it saw (checked during play).
func TestP8ClockSkew(t *testing.T) {
	runSim(t, runs(20, 5000), simOpts{skew: true, drop: 0.1, delay: 0.05, steps: 60},
		func(s *sim, sd int64, run int) { s.noLostWrite(sd, run) })
}

// P9. Forget reaches everyone: secrets noted and forgotten during a partition are on no replica,
// in no file, op or search index, after heal; also when an unredacted copy arrives late.
func TestP9ForgetReachesEveryone(t *testing.T) {
	runSim(t, runs(20, 5000), simOpts{secrets: true, drop: 0.1, delay: 0.15, repeat: 0.05, steps: 60},
		func(s *sim, sd int64, run int) { s.forgotten(sd, run) })
}
