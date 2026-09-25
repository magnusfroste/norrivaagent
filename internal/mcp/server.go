// Package mcp lends the agent's tools to another agent.
//
// `norriva mcp` speaks the Model Context Protocol over stdin/stdout, which is
// how opencode, Claude Code, Kilo and Hermes attach local tool servers. The
// tools are the same ones the built-in loop uses — the linked folder and the
// signed-in person's Norriva data — so the story is not "install our agent"
// but "the agent you already run can now reach Norriva, as you".
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/magnusfroste/norrivaagent/internal/config"
	"github.com/magnusfroste/norrivaagent/internal/tools"
)

const protocolVersion = "2025-03-26"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs until stdin closes. Each line in is one JSON-RPC message; each
// line out is one reply. Notifications (no id) get no reply.
func Serve(cfg *config.Config, ws *config.Workspace, in io.Reader, out io.Writer, version string) error {
	// Until the client introduces itself, the log names the transport.
	set := tools.Set(cfg, ws, "mcp")
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			enc.Encode(response{JSONRPC: "2.0", Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if req.ID == nil {
			continue // a notification: initialized, cancelled, …
		}
		res := response{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			// The client says who it is ("claude-code", "opencode", …). That name
			// goes on every activity line, so the team can see which agent acted.
			var p struct {
				ClientInfo struct {
					Name string `json:"name"`
				} `json:"clientInfo"`
			}
			if json.Unmarshal(req.Params, &p) == nil && p.ClientInfo.Name != "" {
				set = tools.Set(cfg, ws, p.ClientInfo.Name)
			}
			res.Result = map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "norriva", "version": version},
				"instructions": "Tools for the user's Norriva account and their linked folder. Norriva's tables are " +
					"the shared truth; files in the folder are inflow. Start with norriva_tables: the table and " +
					"column comments say what each is for. Put what a file contains into every table that fits — " +
					"a result report fills the actual column of budget_lines (match item and month; update, never " +
					"duplicate; a null actual is a gap to fill), a meeting note becomes a note, its companies or " +
					"people customers, its deal an opportunity at the implied stage. Look at existing rows before " +
					"writing, and read back after writing to check nothing is missing. The source column is set " +
					"to \"agent\" for you; every call is logged for the person's team.",
			}
		case "ping":
			res.Result = map[string]any{}
		case "tools/list":
			list := make([]map[string]any, 0, len(set))
			for _, t := range set {
				list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
			}
			res.Result = map[string]any{"tools": list}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				res.Error = &rpcError{-32602, "invalid params"}
				break
			}
			t := tools.Find(set, p.Name)
			if t == nil {
				res.Error = &rpcError{-32602, fmt.Sprintf("no tool named %s", p.Name)}
				break
			}
			text, err := t.Run(p.Arguments)
			// A failed tool is a result the model reads, not a protocol error.
			isErr := err != nil
			if isErr {
				text = err.Error()
			}
			res.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			}
		default:
			res.Error = &rpcError{-32601, "method not found: " + req.Method}
		}
		if err := enc.Encode(res); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "norriva mcp:", err)
		return err
	}
	return nil
}
