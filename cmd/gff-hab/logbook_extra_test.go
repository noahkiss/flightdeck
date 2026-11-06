package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/lbextra"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
)

// fakeEmbeddingsKey is the bearer key fakeEmbeddings wants, printed by the test's key_command.
const fakeEmbeddingsKey = "test-embeddings-key"

// fakeEmbeddings serves the embeddings shape with lbextra.FakeEngine's vectors at
// <url>/v1/embeddings, to a request with the bearer key fakeEmbeddingsKey.
func fakeEmbeddings(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fakeEmbeddingsKey {
			http.Error(w, "key", http.StatusUnauthorized)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		vs, _ := lbextra.FakeEngine{}.Embed(r.Context(), req.Input)
		type d struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []d `json:"data"`
		}
		for i, v := range vs {
			out.Data = append(out.Data, d{i, v})
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func semanticFind(sock, words string) (api.LogbookRecords, string, error) {
	p, _ := json.Marshal(api.LogbookFindParams{Words: words, Semantic: true})
	out, err := exec.Command(bin, "api", "call", "-socket", sock, "logbook.find", string(p)).CombinedOutput()
	var r api.LogbookRecords
	if err == nil {
		err = json.Unmarshal(out, &r)
	}
	return r, string(out), err
}

// TestLogbookSemanticTwoHabs: with no hab that has vector search, a semantic find answers the
// stated message; once a peer with an index is online, the find goes there, and the ranked
// records come back filtered by this replica (logbook.md 13, 18 row 9).
func TestLogbookSemanticTwoHabs(t *testing.T) {
	d := shortDir(t)
	sockA, sockB := filepath.Join(d, "a.sock"), filepath.Join(d, "b.sock")
	path := sshStandIn(t, map[string]string{"hab-b": sockB})
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	writePeers(t, filesA, "bee", "hab-b")
	writePeers(t, filesB, "ay", "hab-nowhere")
	cfgB := filepath.Join(d, "hab-b.toml")
	if err := os.WriteFile(cfgB, []byte("[logbook.vectors]\nbase_url = \""+fakeEmbeddings(t)+"/v1\"\nmodel = \"fake\"\nkey_command = [\"printf\", \""+fakeEmbeddingsKey+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	peerHab(t, sockA, filepath.Join(d, "sa"), filesA, path, "host-a")
	if slices.Contains(welcome(t, sockA).Capabilities, vectorsCapability) {
		t.Fatal("a hab without [logbook.vectors] lists the capability")
	}
	if _, out, err := semanticFind(sockA, "anything"); err == nil || !strings.Contains(out, lbsvc.NoVectors) {
		t.Fatalf("no vector hab: %v %s", err, out)
	}

	t.Setenv("GFF_TEST_VECTOR_SYNC", "100ms")
	peerHab(t, sockB, filepath.Join(d, "sb"), filesB, path, "host-b", "-config", cfgB)
	if !slices.Contains(welcome(t, sockB).Capabilities, vectorsCapability) {
		t.Fatal("no logbook-vectors capability")
	}
	for _, title := range []string{"rotate the signing certificate", "tidy the build cache", "write the release notes"} {
		p, _ := json.Marshal(api.LogbookWriteParams{IdempotencyKey: "k-" + title, Changes: []api.LogbookChange{
			{Op: api.LogbookChangeOpCreate, Type: "task", Title: title}}})
		if out, err := exec.Command(bin, "api", "call", "-socket", sockB, "logbook.write", string(p)).CombinedOutput(); err != nil {
			t.Fatalf("write: %v %s", err, out)
		}
	}
	for _, sock := range []string{sockB, sockA} {
		waitFor(t, "a semantic find on "+filepath.Base(sock), 30*time.Second, func() bool {
			r, _, err := semanticFind(sock, "signing certificate")
			return err == nil && len(r.Records) > 0 && r.Records[0].Title == "rotate the signing certificate"
		})
	}
}
