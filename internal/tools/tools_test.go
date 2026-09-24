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

// The demo says "put your files in the folder". Text is what the model can use;
// a Word file or PDF must be refused with a sentence that says what to do.
func TestBinaryFilesAreRefusedWithAdvice(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "anteckningar.md"), []byte("# Möte 24/9\nÅsa, Örjan, é"), 0o644)
	os.WriteFile(filepath.Join(root, "offert.docx"), append([]byte("PK\x03\x04"), make([]byte, 64)...), 0o644)
	os.WriteFile(filepath.Join(root, "bild.png"), []byte("\x89PNG\r\n\x1a\n\xff\xfe\x80"), 0o644)
	read := Find(fileTools(&config.Workspace{Name: "demo", Path: root}), "read_file")

	if out, err := read.Run(map[string]any{"path": "anteckningar.md"}); err != nil || !strings.Contains(out, "Örjan") {
		t.Fatalf("UTF-8 text must read as is: %q %v", out, err)
	}
	for _, f := range []string{"offert.docx", "bild.png"} {
		_, err := read.Run(map[string]any{"path": f})
		if err == nil || !strings.Contains(err.Error(), "export it as text") {
			t.Errorf("%s should be refused with advice, got %v", f, err)
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
		if r.Method == http.MethodGet { // the schema lookup that precedes an insert
			w.Write([]byte(`{"definitions":{}}`))
			return
		}
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
	post := (*seen)[len(*seen)-1]
	if post.Method != http.MethodPost || post.Header.Get("Prefer") != "return=representation" {
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

func TestRowsTheAgentWritesAreMarkedAsItsOwn(t *testing.T) {
	var got []map[string]any
	cfg, _ := fakeNorriva(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/v1/" {
			w.Write([]byte(`{"definitions":{"notes":{"description":"Notes.","properties":{"id":{},"title":{},"source":{}}},"plain":{"properties":{"id":{}}}}}`))
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`[]`))
	})
	set := Set(cfg, nil)
	Find(set, "norriva_insert").Run(map[string]any{"table": "notes", "rows": []any{map[string]any{"title": "a"}, map[string]any{"title": "b", "source": "import"}}})
	if got[0]["source"] != "agent" {
		t.Fatalf("a row without a source must be stamped agent, got %v", got[0]["source"])
	}
	if got[1]["source"] != "import" {
		t.Fatal("a source the model set deliberately must be kept")
	}
	got = nil
	Find(set, "norriva_insert").Run(map[string]any{"table": "plain", "rows": []any{map[string]any{"id": 1}}})
	if _, has := got[0]["source"]; has {
		t.Fatal("a table without a source column must be left alone")
	}
	// And the table list carries the comment, which is what tells a model what a table is for.
	out, _ := Find(set, "norriva_tables").Run(nil)
	if !strings.Contains(out, "notes (id, source, title) — Notes.") {
		t.Fatalf("table list should show columns and comment: %q", out)
	}
}

func TestSchemaFallsBackToNorrivasOwnDescription(t *testing.T) {
	// Supabase reserves the OpenAPI root for secret keys; a person's session
	// gets 401 there and must be answered by norriva_schema() instead.
	cfg, _ := fakeNorriva(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/v1/" && r.Method == http.MethodGet:
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"Secret API key required"}`))
		case r.URL.Path == "/rest/v1/rpc/norriva_schema":
			w.Write([]byte(`[{"name":"notes","description":"Notes.","columns":[{"name":"id"},{"name":"source"}]}]`))
		default:
			w.Write([]byte(`[]`))
		}
	})
	out, err := Find(Set(cfg, nil), "norriva_tables").Run(nil)
	if err != nil || !strings.Contains(out, "notes (id, source) — Notes.") {
		t.Fatalf("expected the RPC description, got %q, %v", out, err)
	}
}

func TestEveryToolCallLeavesALineInTheActivityLog(t *testing.T) {
	var logged []map[string]string
	done := make(chan struct{}, 8)
	cfg, _ := fakeNorriva(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/v1/":
			w.Write([]byte(`{"definitions":{"agent_activity":{"properties":{"tool":{},"summary":{}}},"notes":{"properties":{"id":{},"title":{}}}}}`))
		case r.URL.Path == "/rest/v1/agent_activity":
			var rows []map[string]string
			json.NewDecoder(r.Body).Decode(&rows)
			logged = append(logged, rows...)
			w.WriteHeader(201)
			done <- struct{}{}
		default:
			w.Write([]byte(`[{"id":1}]`))
		}
	})
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("private contents"), 0o644)
	set := Set(cfg, &config.Workspace{Name: "demo", Path: root})
	Find(set, "read_file").Run(map[string]any{"path": "a.txt"})
	Find(set, "norriva_query").Run(map[string]any{"table": "notes", "filter": "id=eq.1"})
	<-done
	<-done
	if len(logged) != 2 {
		t.Fatalf("two calls, two log lines; got %d", len(logged))
	}
	for _, l := range logged {
		if strings.Contains(l["summary"], "private contents") {
			t.Fatal("the log must name the file, never quote it")
		}
		if l["device"] == "" || l["agent"] != "norriva" {
			t.Fatalf("a log line says which agent on which machine: %v", l)
		}
	}
}
