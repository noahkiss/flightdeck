package gitstatus

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/structure"
)

// Defaults for Config.
const (
	DefaultInterval    = 60 * time.Second
	DefaultPoll        = 2 * time.Second
	DefaultDebounce    = 500 * time.Millisecond
	DefaultTimeout     = 2 * time.Second
	DefaultConcurrency = 4
)

// Store is what the Watcher needs from the structure store.
type Store interface {
	ProjectDirs() []structure.ProjectDir
	SetProjectRepo(id string, r structure.RepoState) (bool, error)
}

// Config configures a Watcher. Store is required; zero durations take the defaults.
type Config struct {
	Store       Store
	Reader      Reader
	Interval    time.Duration // read every project this often
	Poll        time.Duration // stat each project's HEAD and refs this often; negative turns it off
	Debounce    time.Duration // a Refresh waits this long, so a burst of hooks gives one read
	Timeout     time.Duration // the limit for one read, all its git runs together
	Concurrency int           // reads at once over all projects
	Logf        func(string, ...any)

	afterRead func(error) // tests: called after each read with its error
}

// Watcher keeps each project's repository state fresh in the store. Each project has at most
// one read running; a request during a read runs one more read after it. Reads run off the
// caller's goroutine, and the store is only called briefly before and after a read, so a slow or
// hung git never blocks the store.
type Watcher struct {
	cfg  Config
	sem  chan struct{}
	stop chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	projects map[string]*project
}

type project struct {
	timer   *time.Timer // a pending debounced read
	running bool
	again   bool     // a request arrived during the read
	watch   []string // files from the last read
	sig     string   // their stat signature after the last read
	failing bool     // the last read failed; logged once until a read succeeds
}

// New starts a Watcher. It reads every project once at once. Close stops it.
func New(cfg Config) *Watcher {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Poll == 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = DefaultDebounce
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	w := &Watcher{
		cfg:      cfg,
		sem:      make(chan struct{}, cfg.Concurrency),
		stop:     make(chan struct{}),
		projects: map[string]*project{},
	}
	w.wg.Add(1)
	go w.loop()
	return w
}

// Close stops the timers and waits for running reads to end. A running git is killed.
func (w *Watcher) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	for _, p := range w.projects {
		if p.timer != nil && p.timer.Stop() {
			p.timer = nil
			w.wg.Done()
		}
	}
	w.mu.Unlock()
	close(w.stop)
	w.wg.Wait()
}

// Refresh asks for a read of the project after the debounce. Many requests within the
// debounce give one read. It never blocks.
func (w *Watcher) Refresh(id string) {
	if id == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	p := w.project(id)
	if p.timer != nil {
		return
	}
	w.wg.Add(1)
	p.timer = time.AfterFunc(w.cfg.Debounce, func() {
		defer w.wg.Done()
		w.mu.Lock()
		p.timer = nil
		w.mu.Unlock()
		w.start(id)
	})
}

// RefreshNow starts a read of the project at once, with no debounce.
func (w *Watcher) RefreshNow(id string) { w.start(id) }

func (w *Watcher) project(id string) *project {
	p := w.projects[id]
	if p == nil {
		p = &project{}
		w.projects[id] = p
	}
	return p
}

func (w *Watcher) loop() {
	defer w.wg.Done()
	w.all()
	full := time.NewTicker(w.cfg.Interval)
	defer full.Stop()
	var poll <-chan time.Time
	if w.cfg.Poll > 0 {
		t := time.NewTicker(w.cfg.Poll)
		defer t.Stop()
		poll = t.C
	}
	for {
		select {
		case <-w.stop:
			return
		case <-full.C:
			w.all()
		case <-poll:
			w.poll()
		}
	}
}

// all starts a read of every project, and forgets projects the store no longer has.
func (w *Watcher) all() {
	list := w.cfg.Store.ProjectDirs()
	w.prune(list)
	for _, pd := range list {
		w.start(pd.ID)
	}
}

// poll stats the files each project's last read named and starts a read where they changed.
func (w *Watcher) poll() {
	list := w.cfg.Store.ProjectDirs()
	w.prune(list)
	for _, pd := range list {
		w.mu.Lock()
		p := w.projects[pd.ID]
		var watch []string
		var sig string
		if p != nil && !p.running {
			watch, sig = p.watch, p.sig
		}
		w.mu.Unlock()
		if len(watch) > 0 && statSig(watch) != sig {
			w.start(pd.ID)
		}
	}
}

func (w *Watcher) prune(list []structure.ProjectDir) {
	keep := map[string]bool{}
	for _, pd := range list {
		keep[pd.ID] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, p := range w.projects {
		if !keep[id] && !p.running && p.timer == nil {
			delete(w.projects, id)
		}
	}
}

// start runs a read of the project on its own goroutine, unless one is running: then it marks
// the project so one more read follows.
func (w *Watcher) start(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	p := w.project(id)
	if p.running {
		p.again = true
		return
	}
	p.running = true
	w.wg.Add(1)
	go w.run(id, p)
}

func (w *Watcher) run(id string, p *project) {
	defer w.wg.Done()
	for {
		select {
		case w.sem <- struct{}{}:
		case <-w.stop:
			w.mu.Lock()
			p.running = false
			w.mu.Unlock()
			return
		}
		w.read(id, p)
		<-w.sem
		w.mu.Lock()
		if !p.again || w.closed {
			p.running = false
			w.mu.Unlock()
			return
		}
		p.again = false
		w.mu.Unlock()
	}
}

// read reads the project's repository and stores the state. A failure keeps the last values
// and is logged once, until a read succeeds again.
func (w *Watcher) read(id string, p *project) {
	dir, ok := w.dirOf(id)
	if !ok {
		return // the project is gone
	}
	var res Result
	var err error
	if dir != "" {
		ctx, cancel := context.WithTimeout(context.Background(), w.cfg.Timeout)
		go func() {
			select {
			case <-w.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		res, err = w.cfg.Reader.Read(ctx, dir)
		cancel()
	}
	if w.cfg.afterRead != nil {
		w.cfg.afterRead(err)
	}
	w.mu.Lock()
	if err != nil {
		first := !p.failing
		p.failing = true
		w.mu.Unlock()
		if first {
			w.cfg.Logf("git status of project %s in %s: %v; keeping the last values", id, dir, err)
		}
		return
	}
	p.failing = false
	p.watch = res.Watch
	p.sig = statSig(res.Watch)
	w.mu.Unlock()
	if _, err := w.cfg.Store.SetProjectRepo(id, res.State); err != nil {
		w.cfg.Logf("git status of project %s: %v", id, err)
	}
}

// dirOf picks the folder to read for a project: its most recently used working directory that
// still exists. "" means none exists (the state is cleared); ok is false when the project is gone.
func (w *Watcher) dirOf(id string) (string, bool) {
	for _, pd := range w.cfg.Store.ProjectDirs() {
		if pd.ID != id {
			continue
		}
		for _, d := range pd.Dirs {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				return d, true
			}
		}
		return "", true
	}
	return "", false
}

// statSig is a cheap signature of the files: each one's size and modification time, or that it
// is missing.
func statSig(files []string) string {
	var b strings.Builder
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			b.WriteString("-;")
			continue
		}
		fmt.Fprintf(&b, "%d:%d;", fi.Size(), fi.ModTime().UnixNano())
	}
	return b.String()
}

// idle reports that no read is running or waiting; tests use it.
func (w *Watcher) idle() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.projects {
		if p.running || p.timer != nil {
			return false
		}
	}
	return true
}
