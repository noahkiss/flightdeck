package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/gitstatus"
	"github.com/noahkiss/flightdeck/internal/speech"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// speechVocabTTL is how long a folder's file list serves the vocabulary fix-up before git runs
// again; speechVocabTimeout bounds one git run.
const (
	speechVocabTTL     = time.Minute
	speechVocabTimeout = 3 * time.Second
)

// speechEnv is what speechSetup reads from the process; tests replace it.
type speechEnv struct {
	getenv     func(string) string
	executable func() (string, error)
	memory     uint64 // host memory in bytes; 0 unknown
}

// speechSetup decides whether this hab runs the speech engine (voice.md, Design): on when the
// helper is installed and the [speech] section of the hab's config file turns it on, or leaves
// it to the default (on with at least 4 GB of memory). It returns nil, with the reason logged,
// when the engine is off. A config file that does not parse is an error.
func speechSetup(configPath, state string, store *structure.Store, env speechEnv, logf func(string, ...any)) (*speech.Engine, error) {
	set, err := speech.LoadSettings(configPath)
	if err != nil {
		return nil, err
	}
	on, why := set.On(env.memory)
	if !on {
		logf("speech: engine %s", why)
		return nil, nil
	}
	helper := speech.FindHelper(set, env.getenv, env.executable)
	if helper == "" {
		logf("speech: engine off: %s is not installed", speech.HelperName)
		return nil, nil
	}
	model := speech.Parakeet
	if set.ModelURL != "" {
		model.URL = set.ModelURL
	}
	v := &speechVocab{store: store, cache: map[string]vocabEntry{}}
	logf("speech: engine %s, helper %s", why, helper)
	return speech.New(speech.Config{
		Helper: helper, State: state, Model: model, Threads: set.ThreadCount(),
		Vocabulary: v.terms, Logf: logf,
	}), nil
}

// speechVocab builds a session's vocabulary for the fix-up: the file and folder names git knows
// in the session's project folder, tracked or not ignored.
type speechVocab struct {
	store *structure.Store
	git   gitstatus.Reader

	mu    sync.Mutex
	cache map[string]vocabEntry // by folder
}

type vocabEntry struct {
	terms []string
	at    time.Time
}

func (v *speechVocab) terms(ctx context.Context, session string) ([]string, bool) {
	s, ok := v.store.Session(session)
	if !ok || s.Project == "" {
		return nil, false
	}
	dir := ""
	for _, pd := range v.store.ProjectDirs() {
		if pd.ID != s.Project {
			continue
		}
		for _, d := range pd.Dirs {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				dir = d
				break
			}
		}
	}
	if dir == "" {
		return nil, false
	}
	v.mu.Lock()
	e, ok := v.cache[dir]
	v.mu.Unlock()
	if ok && time.Since(e.at) < speechVocabTTL {
		return e.terms, true
	}
	ctx, cancel := context.WithTimeout(ctx, speechVocabTimeout)
	defer cancel()
	out, err := v.git.Run(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, false
	}
	terms := speech.Terms(strings.Split(out, "\x00"))
	v.mu.Lock()
	v.cache[dir] = vocabEntry{terms: terms, at: time.Now()}
	if len(v.cache) > 64 {
		for k := range v.cache {
			if k != dir {
				delete(v.cache, k)
				break
			}
		}
	}
	v.mu.Unlock()
	return terms, true
}
