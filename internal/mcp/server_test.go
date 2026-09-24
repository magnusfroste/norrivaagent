package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magnusfroste/norrivaagent/internal/config"
)

// Drives the server the way a CLI agent does: one JSON-RPC message per line
// in, one per line out, and no reply at all to notifications.
func talk(t *testing.T, ws *config.Workspace, lines ...string) []map[string]any {
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := Serve(&config.Config{}, ws, in, &out, "test"); err != nil {
		t.Fatal(err)
	}
	var replies []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not one JSON message per line: %q", l)
		}
		replies = append(replies, m)
	}
	return replies
}

// Over MCP the caller is another agent, and the activity log must say which:
// the name from the initialize handshake, not a generic "norriva".
func TestActivityNamesTheCallingAgent(t *testing.T) {
	var agents []string
	done := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/v1/":
			w.Write([]byte(`{"definitions":{"agent_activity":{"properties":{"tool":{}}}}}`))
		case "/rest/v1/agent_activity":
			var rows []map[string]string
			json.NewDecoder(r.Body).Decode(&rows)
			for _, row := range rows {
				agents = append(agents, row["agent"])
			}
			w.WriteHeader(201)
			done <- struct{}{}
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	cfg := &config.Config{SupabaseURL: srv.URL, AnonKey: "anon", AccessToken: "user-jwt"}
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"claude-code","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"a.txt"}}}`,
	}, "\n") + "\n")
	var out bytes.Buffer
	if err := Serve(cfg, &config.Workspace{Name: "demo", Path: root}, in, &out, "test"); err != nil {
		t.Fatal(err)
	}
	<-done
	if len(agents) != 1 || agents[0] != "claude-code" {
		t.Fatalf("the log should name the MCP client, got %v", agents)
	}
}

func TestHandshakeAndToolList(t *testing.T) {
	root := t.TempDir()
	replies := talk(t, &config.Workspace{Name: "demo", Path: root},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(replies) != 2 {
		t.Fatalf("a notification must get no reply; got %d replies", len(replies))
	}
	init := replies[0]["result"].(map[string]any)
	if init["protocolVersion"] != protocolVersion {
		t.Fatal("initialize must state the protocol version")
	}
	tools := replies[1]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	// Not signed in: file tools only. The Norriva tools appear with a session.
	if strings.Join(names, ",") != "list_files,read_file,write_file" {
		t.Fatalf("unexpected tools for a folder without a session: %v", names)
	}
}

func TestToolErrorsAreResultsNotProtocolErrors(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("A"), 0o644)
	replies := talk(t, &config.Workspace{Name: "demo", Path: root},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"a.txt"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"../nope"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"unknown/method"}`,
	)
	ok := replies[0]["result"].(map[string]any)
	if ok["isError"] != false || ok["content"].([]any)[0].(map[string]any)["text"] != "A" {
		t.Fatalf("a good call returns the text: %v", ok)
	}
	// A refused path is something the model should read and react to, so it
	// travels as a result with isError, not as a JSON-RPC error that ends the
	// conversation.
	refused := replies[1]["result"].(map[string]any)
	if refused["isError"] != true {
		t.Fatal("a tool failure must be a result with isError=true")
	}
	if replies[2]["error"] == nil || replies[3]["error"] == nil {
		t.Fatal("an unknown tool or method is a protocol error")
	}
}

func TestGarbageInDoesNotStopTheServer(t *testing.T) {
	replies := talk(t, nil,
		`this is not json`,
		`{"jsonrpc":"2.0","id":9,"method":"ping"}`,
	)
	if len(replies) != 2 || replies[1]["id"].(float64) != 9 {
		t.Fatalf("after a parse error the next message must still be answered: %v", replies)
	}
}
