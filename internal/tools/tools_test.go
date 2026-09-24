package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magnusfroste/norrivaagent/internal/config"
)

// The fence is the whole promise of `norriva link`: this folder, and nowhere
// else. Every way out that a path can express must end at the fence.
func TestFileToolsStayInsideTheLinkedFolder(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside-"+filepath.Base(root))
	os.WriteFile(outside, []byte("secret"), 0o644)
	defer os.Remove(outside)
	os.WriteFile(filepath.Join(root, "notes.md"), []byte("inside"), 0o644)

	set := fileTools(&config.Workspace{Name: "demo", Path: root})
	read := Find(set, "read_file")

	if out, err := read.Run(map[string]any{"path": "notes.md"}); err != nil || out != "inside" {
		t.Fatalf("a file inside should read: %q, %v", out, err)
	}
	for _, escape := range []string{
		"../" + filepath.Base(outside),
		outside, // absolute
		"sub/../../" + filepath.Base(outside),
	} {
		if _, err := read.Run(map[string]any{"path": escape}); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("%q should be refused as outside the folder, got %v", escape, err)
		}
	}
}

func TestWriteCreatesFoldersButOnlyInside(t *testing.T) {
	root := t.TempDir()
	set := fileTools(&config.Workspace{Name: "demo", Path: root})
	write := Find(set, "write_file")

	if _, err := write.Run(map[string]any{"path": "a/b/c.txt", "content": "x"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a/b/c.txt")); string(b) != "x" {
		t.Fatal("the write did not land where it said")
	}
	if _, err := write.Run(map[string]any{"path": "../escaped.txt", "content": "x"}); err == nil {
		t.Fatal("a write outside the folder must be refused")
	}
}

// A Norriva stand-in: records what PostgREST would have received.
func fakeNorriva(t *testing.T, handler http.HandlerFunc) (*config.Config, *[]*http.Request) {
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return &config.Config{SupabaseURL: srv.URL, AnonKey: "anon", AccessToken: "user-jwt"}, &seen
}

func TestNorrivaToolsCarryTheUsersOwnSession(t *testing.T) {
	cfg, seen := fakeNorriva(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1}]`))
	})
	set := Set(cfg, nil)
	if Find(set, "list_files") != nil {
		t.Fatal("no folder linked, so no file tools should be offered")
	}
	if _, err := Find(set, "norriva_query").Run(map[string]any{"table": "notes", "filter": "source=eq.agent"}); err != nil {
		t.Fatal(err)
	}
	r := (*seen)[0]
	// The anon key identifies the project; the Bearer token is the person. Row
	// level security only works if both are exactly this way round.
	if r.Header.Get("apikey") != "anon" || r.Header.Get("Authorization") != "Bearer user-jwt" {
		t.Fatalf("wrong credentials on the wire: apikey=%q auth=%q", r.Header.Get("apikey"), r.Header.Get("Authorization"))
	}
	if r.URL.Path != "/rest/v1/notes" || r.URL.Query().Get("source") != "eq.agent" || r.URL.Query().Get("limit") != "50" {
		t.Fatalf("unexpected request: %s", r.URL)
	}
}

func TestUpdateWithoutAFilterIsRefusedBeforeItReachesTheWire(t *testing.T) {
	cfg, seen := fakeNorriva(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err := Find(Set(cfg, nil), "norriva_update").Run(map[string]any{"table": "notes", "filter": "  ", "patch": map[string]any{"body": "x"}})
	if err == nil || !strings.Contains(err.Error(), "without a filter") {
		t.Fatalf("an unfiltered update would rewrite the whole table; got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatal("the request must not be sent at all")
	}
}

func TestInsertReturnsWhatWasStored(t *testing.T) {
	cfg, seen := fakeNorriva(t, func(w http.ResponseWriter, r *http.Request) {
		var rows []map[string]any
		json.NewDecoder(r.Body).Decode(&rows)
		rows[0]["id"] = "generated"
		json.NewEncoder(w).Encode(rows)
	})
	out, err := Find(Set(cfg, nil), "norriva_insert").Run(map[string]any{"table": "notes", "rows": []any{map[string]any{"title": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"generated"`) {
		t.Fatalf("the model needs the ids the database assigned: %s", out)
	}
	if (*seen)[0].Header.Get("Prefer") != "return=representation" {
		t.Fatal("without Prefer: return=representation PostgREST answers with nothing")
	}
}

func TestErrorsFromNorrivaAreReadable(t *testing.T) {
	cfg, _ := fakeNorriva(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"message":"column \"nme\" does not exist","hint":"Perhaps you meant \"name\""}`))
	})
	_, err := Find(Set(cfg, nil), "norriva_query").Run(map[string]any{"table": "notes", "filter": "nme=eq.x"})
	if err == nil || !strings.Contains(err.Error(), "Perhaps you meant") {
		t.Fatalf("the hint is the useful part and must survive: %v", err)
	}
}
