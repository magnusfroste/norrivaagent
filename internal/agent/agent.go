// Package agent is the loop: send the conversation to the model, run whatever
// tools it asks for, send the results back, until it answers in words.
//
// Deliberately small. The interesting part of this program is not the loop —
// every agent has one — but where the tools reach: a folder the person chose
// and the data the person is allowed to see. The model is reached through an
// OpenAI-compatible endpoint, which Norriva hands out at login together with
// the credential for it; nothing here knows which model is behind it.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/magnusfroste/norrivaagent/internal/config"
	"github.com/magnusfroste/norrivaagent/internal/tools"
)

const maxTurns = 16

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Agent struct {
	cfg   *config.Config
	ws    *config.Workspace
	tools []tools.Tool
	out   io.Writer
	// Trace prints each tool call and a line of its result, so the person can
	// see what the agent is doing to their files and their data as it happens.
	Trace bool
}

func New(cfg *config.Config, ws *config.Workspace, out io.Writer) *Agent {
	return &Agent{cfg: cfg, ws: ws, tools: tools.Set(cfg, ws), out: out, Trace: true}
}

func (a *Agent) systemPrompt() string {
	var b strings.Builder
	b.WriteString("You are the Norriva agent, running on the user's own computer.\n")
	if a.cfg.Email != "" {
		fmt.Fprintf(&b, "You act as %s; Norriva's permissions for that account apply to everything you read or write there.\n", a.cfg.Email)
	}
	if a.ws != nil {
		fmt.Fprintf(&b, "You may read and write files inside the linked folder %q (%s) and nowhere else.\n", a.ws.Name, a.ws.Path)
	} else {
		b.WriteString("No local folder is linked; you can only work with Norriva data.\n")
	}
	b.WriteString("Norriva is the shared data the user's organisation works in. Before writing to a table, look at it first so your rows match its shape. ")
	b.WriteString("Say what you did, briefly, when you are done. Do not narrate tool calls the user can already see.")
	return b.String()
}

// Run takes one instruction and returns the model's final answer, having run
// whatever tools it needed along the way.
func (a *Agent) Run(ctx context.Context, prompt string, history []Message) ([]Message, error) {
	if a.cfg.Model.BaseURL == "" {
		return nil, errors.New("no model configured — `norriva login` hands one out, or set NORRIVA_MODEL_URL and NORRIVA_MODEL_KEY")
	}
	msgs := append([]Message{{Role: "system", Content: a.systemPrompt()}}, history...)
	msgs = append(msgs, Message{Role: "user", Content: prompt})

	for turn := 0; turn < maxTurns; turn++ {
		reply, err := a.complete(ctx, msgs)
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, *reply)
		if len(reply.ToolCalls) == 0 {
			fmt.Fprintln(a.out, strings.TrimSpace(reply.Content))
			return msgs[1:], nil
		}
		for _, call := range reply.ToolCalls {
			result := a.call(call)
			msgs = append(msgs, Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: result})
		}
	}
	return msgs[1:], fmt.Errorf("stopped after %d tool rounds without a final answer", maxTurns)
}

func (a *Agent) call(call ToolCall) string {
	t := tools.Find(a.tools, call.Function.Name)
	if t == nil {
		return fmt.Sprintf("error: no tool named %s", call.Function.Name)
	}
	var args map[string]any
	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "error: arguments are not valid JSON: " + err.Error()
		}
	}
	if a.Trace {
		fmt.Fprintf(a.out, "  ▸ %s %s\n", t.Name, oneLine(call.Function.Arguments, 90))
	}
	out, err := t.Run(args)
	if err != nil {
		if a.Trace {
			fmt.Fprintf(a.out, "    ✗ %s\n", err)
		}
		return "error: " + err.Error()
	}
	if a.Trace {
		fmt.Fprintf(a.out, "    %s\n", oneLine(out, 100))
	}
	return out
}

func (a *Agent) complete(ctx context.Context, msgs []Message) (*Message, error) {
	type fn struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	type toolDef struct {
		Type     string `json:"type"`
		Function fn     `json:"function"`
	}
	defs := make([]toolDef, 0, len(a.tools))
	for _, t := range a.tools {
		defs = append(defs, toolDef{Type: "function", Function: fn{t.Name, t.Description, t.Schema}})
	}
	model := a.cfg.Model.Name
	if model == "" {
		model = "gpt-5.6-luna"
	}
	// Newer OpenAI models refuse function tools on /chat/completions unless
	// reasoning is switched off explicitly ("use /v1/responses or set
	// reasoning_effort to none"). The tools are the point here, so: none.
	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs, "tools": defs, "reasoning_effort": "none"})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.Model.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.Model.APIKey)
	res, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the model: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("the model endpoint answered %d: %s", res.StatusCode, oneLine(string(raw), 300))
	}
	var parsed struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("the model endpoint answered something that is not a chat completion: %s", oneLine(string(raw), 200))
	}
	return &parsed.Choices[0].Message, nil
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
