// Package tools is what the agent can do, written once and used twice: by the
// built-in agent loop, and by the MCP server that lends the same abilities to
// whatever CLI agent the person already runs.
//
// Two families. File tools are fenced to the linked folder — a path that
// resolves outside it is refused before anything is opened. Norriva tools go
// through the signed-in person's session, so row-level security decides what
// they may touch.
package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/magnusfroste/norrivaagent/internal/config"
	"github.com/magnusfroste/norrivaagent/internal/norriva"
)

// Tool is one ability: a name the model calls it by, a description it reads
// to decide when, a JSON Schema for the arguments, and the code.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Run         func(args map[string]any) (string, error)
}

// Set builds the tools for one workspace and one signed-in session. ws may be
// nil, in which case only the Norriva tools are offered — a session without a
// folder can still read and write the shared data. agent names who is calling
// — "norriva" for the built-in loop, the client's own name over MCP — so the
// activity log can say which agent did what, not just from which machine.
func Set(cfg *config.Config, ws *config.Workspace, agent string) []Tool {
	var out []Tool
	if ws != nil {
		out = append(out, fileTools(ws)...)
	}
	if cfg.LoggedIn() {
		c := norriva.New(cfg)
		out = append(out, norrivaTools(c)...)
		// Every call, from either family, leaves a line in Norriva's activity
		// log. That is the traceability a team wants from an agent it cannot
		// see: what it touched, from which machine, when.
		device, _ := os.Hostname()
		if agent == "" {
			agent = "norriva"
		}
		for i := range out {
			out[i] = traced(out[i], c, agent, device)
		}
	}
	return out
}

func traced(t Tool, c *norriva.Client, agent, device string) Tool {
	run := t.Run
	t.Run = func(args map[string]any) (string, error) {
		started := time.Now()
		res, err := run(args)
		summary := summarize(t.Name, args, res, err)
		if time.Since(started) > 0 {
			go c.Activity(agent, device, t.Name, summary)
		}
		return res, err
	}
	return t
}

// summarize is what the log says. Names of things, never their contents.
func summarize(tool string, args map[string]any, res string, err error) string {
	var s string
	switch tool {
	case "list_files", "read_file", "write_file":
		s = str(args["path"])
		if s == "" {
			s = "/"
		}
	case "norriva_tables":
		s = "listed tables"
	case "norriva_query", "norriva_insert", "norriva_update":
		s = str(args["table"])
		if rows, ok := args["rows"].([]any); ok {
			s = fmt.Sprintf("%s ×%d", s, len(rows))
		}
		if f := str(args["filter"]); f != "" {
			s += " where " + f
		}
	}
	if err != nil {
		s += " — failed: " + err.Error()
	}
	return s
}

// ---- files -----------------------------------------------------------------

const maxRead = 200 * 1024

// inside resolves rel against the workspace root and refuses anything that
// escapes it. `..`, absolute paths and symlinks out of the folder all end up
// here; the check is on the cleaned absolute path, not on the string.
func inside(ws *config.Workspace, rel string) (string, error) {
	root, err := filepath.Abs(ws.Path)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	abs := filepath.Clean(filepath.Join(root, rel))
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside the linked folder %s", rel, ws.Name)
	}
	return abs, nil
}

func fileTools(ws *config.Workspace) []Tool {
	return []Tool{
		{
			Name:        "list_files",
			Description: fmt.Sprintf("List files and folders inside the linked folder %q (%s). Paths are relative to it.", ws.Name, ws.Path),
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Sub-folder to list; empty for the root"}}}`),
			Run: func(args map[string]any) (string, error) {
				abs, err := inside(ws, str(args["path"]))
				if err != nil {
					return "", err
				}
				entries, err := os.ReadDir(abs)
				if err != nil {
					return "", err
				}
				var lines []string
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".") {
						continue
					}
					if e.IsDir() {
						lines = append(lines, e.Name()+"/")
					} else {
						info, _ := e.Info()
						lines = append(lines, fmt.Sprintf("%s  (%d bytes)", e.Name(), info.Size()))
					}
				}
				sort.Strings(lines)
				if len(lines) == 0 {
					return "(empty)", nil
				}
				return strings.Join(lines, "\n"), nil
			},
		},
		{
			Name:        "read_file",
			Description: "Read a text file inside the linked folder — .md, .txt, .csv, .json and the like. Binary formats (.pdf, .docx, .xlsx, images) are refused. Large files are truncated at 200 kB.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			Run: func(args map[string]any) (string, error) {
				abs, err := inside(ws, str(args["path"]))
				if err != nil {
					return "", err
				}
				b, err := os.ReadFile(abs)
				if err != nil {
					return "", err
				}
				if isBinary(b) {
					return "", fmt.Errorf("%s is not a text file; the agent reads .md, .txt, .csv, .json and similar — export it as text first", str(args["path"]))
				}
				if len(b) > maxRead {
					return string(b[:maxRead]) + "\n…(truncated)", nil
				}
				return string(b), nil
			},
		},
		{
			Name:        "write_file",
			Description: "Write a text file inside the linked folder, creating folders as needed. Overwrites.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			Run: func(args map[string]any) (string, error) {
				abs, err := inside(ws, str(args["path"]))
				if err != nil {
					return "", err
				}
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					return "", err
				}
				content := str(args["content"])
				if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("wrote %d bytes to %s", len(content), str(args["path"])), nil
			},
		},
	}
}

// isBinary is the cheap test editors use: a NUL byte in the first 8 kB, or
// bytes that are not UTF-8. A .docx or .pdf fed to a model as a string is
// noise at best, and the honest answer is "export it as text".
func isBinary(b []byte) bool {
	if len(b) > 8192 {
		b = b[:8192]
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	return !utf8.Valid(b) && !utf8.Valid(b[:len(b)-utf8.UTFMax])
}

// ---- norriva ---------------------------------------------------------------

func norrivaTools(c *norriva.Client) []Tool {
	return []Tool{
		{
			Name:        "norriva_tables",
			Description: "List the Norriva tables this account can see.",
			Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
			Run: func(map[string]any) (string, error) {
				tables, err := c.Tables()
				if err != nil {
					return "", err
				}
				if len(tables) == 0 {
					return "(no tables visible to this account)", nil
				}
				var b strings.Builder
				for _, t := range tables {
					fmt.Fprintf(&b, "%s (%s)", t.Name, strings.Join(t.Columns, ", "))
					if t.Description != "" {
						fmt.Fprintf(&b, " — %s", t.Description)
					}
					b.WriteString("\n")
				}
				return strings.TrimRight(b.String(), "\n"), nil
			},
		},
		{
			Name: "norriva_query",
			Description: "Read rows from a Norriva table. filter is a PostgREST query string, e.g. " +
				"\"status=eq.open&order=created_at.desc\" or \"select=id,name&name=ilike.*acme*\". Returns JSON.",
			Schema: json.RawMessage(`{"type":"object","properties":{"table":{"type":"string"},"filter":{"type":"string","description":"PostgREST query string; may be empty"},"limit":{"type":"integer","description":"Max rows, default 50, max 200"}},"required":["table"]}`),
			Run: func(args map[string]any) (string, error) {
				out, err := c.Select(str(args["table"]), str(args["filter"]), num(args["limit"]))
				if err != nil {
					return "", err
				}
				return string(out), nil
			},
		},
		{
			Name:        "norriva_insert",
			Description: "Insert one or more rows into a Norriva table. rows is a JSON array of objects. Returns the rows as stored, with ids.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"table":{"type":"string"},"rows":{"type":"array","items":{"type":"object"}}},"required":["table","rows"]}`),
			Run: func(args map[string]any) (string, error) {
				rows, err := json.Marshal(args["rows"])
				if err != nil {
					return "", err
				}
				if string(rows) == "null" || string(rows) == "[]" {
					return "", errors.New("rows must be a non-empty JSON array")
				}
				rows = stampSource(c, str(args["table"]), rows)
				out, err := c.Insert(str(args["table"]), rows)
				if err != nil {
					return "", err
				}
				return string(out), nil
			},
		},
		{
			Name:        "norriva_update",
			Description: "Update the rows a PostgREST filter matches, e.g. table=\"tasks\", filter=\"id=eq.42\", patch={\"status\":\"done\"}. A filter is required.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"table":{"type":"string"},"filter":{"type":"string"},"patch":{"type":"object"}},"required":["table","filter","patch"]}`),
			Run: func(args map[string]any) (string, error) {
				patch, err := json.Marshal(args["patch"])
				if err != nil {
					return "", err
				}
				patch = stampSourceOne(c, str(args["table"]), patch)
				out, err := c.Update(str(args["table"]), str(args["filter"]), patch)
				if err != nil {
					return "", err
				}
				return string(out), nil
			},
		},
	}
}

// stampSource marks rows the agent writes as its own. Norriva shows where a
// row came from, and that must not depend on the model: when the table has a
// source column, every row the agent writes says "agent" — even if the model
// put something else there (the first live run wrote the file name, which is
// the activity log's job, not the row's). A table without the column is left
// alone.
func stampSource(c *norriva.Client, table string, rows json.RawMessage) json.RawMessage {
	if !c.HasColumn(table, "source") {
		return rows
	}
	var list []map[string]any
	if json.Unmarshal(rows, &list) != nil {
		return rows
	}
	for _, r := range list {
		r["source"] = "agent"
	}
	out, err := json.Marshal(list)
	if err != nil {
		return rows
	}
	return out
}

// stampSourceOne is stampSource for a single object — the patch of an update.
// A row the agent fills in (an actual from the books) is as much its work as
// a row it creates, and the dashboard shows it the same way. It only touches
// source when the patch already mentions it or the table has the column.
func stampSourceOne(c *norriva.Client, table string, patch json.RawMessage) json.RawMessage {
	if !c.HasColumn(table, "source") {
		return patch
	}
	var obj map[string]any
	if json.Unmarshal(patch, &obj) != nil || obj == nil {
		return patch
	}
	obj["source"] = "agent"
	out, err := json.Marshal(obj)
	if err != nil {
		return patch
	}
	return out
}

// Find returns the tool with that name, or nil.
func Find(set []Tool, name string) *Tool {
	for i := range set {
		if set[i].Name == name {
			return &set[i]
		}
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
