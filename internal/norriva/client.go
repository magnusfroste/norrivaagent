// Package norriva reads and writes the shared data through PostgREST, as the
// signed-in person.
//
// The agent sends the user's own session token, so every row-level security
// rule Norriva has already written applies unchanged: the agent on the laptop
// can do exactly what the person can, no more. That is the whole answer to
// "is it safe to let an agent at our data" — the permission model is Norriva's,
// and this client merely carries the credential that invokes it.
package norriva

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/magnusfroste/norrivaagent/internal/config"
)

type Client struct {
	cfg    *config.Config
	http   *http.Client
	schema map[string]Table // fetched once per process
}

// Table is what PostgREST publishes about one table: its columns and the
// comment its author left. The comment is the part that tells a model what a
// table is *for* — "Notes a person keeps in Norriva" says more than any column.
type Table struct {
	Name        string
	Description string
	Columns     []string
}

func New(cfg *config.Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) do(method, path string, body []byte, prefer string) ([]byte, error) {
	req, err := http.NewRequest(method, c.cfg.SupabaseURL+"/rest/v1/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.cfg.AnonKey)
	req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode == 401 {
		return nil, fmt.Errorf("Norriva rejected the session (401) — run `norriva login`")
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("Norriva answered %d: %s", res.StatusCode, summarizeError(out))
	}
	return out, nil
}

// Schema reads PostgREST's OpenAPI description once and keeps it. A gateway
// that hides the root behind an admin-only route (some do) answers 403 here;
// callers then work without it rather than failing.
func (c *Client) Schema() (map[string]Table, error) {
	if c.schema != nil {
		return c.schema, nil
	}
	req, _ := http.NewRequest(http.MethodGet, c.cfg.SupabaseURL+"/rest/v1/", nil)
	req.Header.Set("apikey", c.cfg.AnonKey)
	req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	req.Header.Set("Accept", "application/openapi+json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		// Supabase reserves the OpenAPI root for secret keys ("Only secret API
		// keys can be used for this endpoint"), so a person's session can never
		// read it. Norriva describes itself instead, through a function any
		// signed-in caller may run.
		return c.schemaFromRPC()
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("the table list is not available to this session (%d)", res.StatusCode)
	}
	var spec struct {
		Definitions map[string]struct {
			Description string                     `json:"description"`
			Properties  map[string]json.RawMessage `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.NewDecoder(res.Body).Decode(&spec); err != nil {
		return nil, err
	}
	out := make(map[string]Table, len(spec.Definitions))
	for name, d := range spec.Definitions {
		cols := make([]string, 0, len(d.Properties))
		for col := range d.Properties {
			cols = append(cols, col)
		}
		sort.Strings(cols)
		out[name] = Table{Name: name, Description: strings.TrimSpace(d.Description), Columns: cols}
	}
	c.schema = out
	return out, nil
}

func (c *Client) schemaFromRPC() (map[string]Table, error) {
	raw, err := c.do(http.MethodPost, "rpc/norriva_schema", []byte("{}"), "")
	if err != nil {
		return nil, fmt.Errorf("the table list is not available to this session: %w", err)
	}
	var list []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Columns     []struct {
			Name string `json:"name"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("norriva_schema answered something unexpected: %w", err)
	}
	out := make(map[string]Table, len(list))
	for _, t := range list {
		cols := make([]string, 0, len(t.Columns))
		for _, col := range t.Columns {
			cols = append(cols, col.Name)
		}
		sort.Strings(cols)
		out[t.Name] = Table{Name: t.Name, Description: strings.TrimSpace(t.Description), Columns: cols}
	}
	c.schema = out
	return out, nil
}

// Activity records one thing the agent did, for the person to see in Norriva.
// Best effort and never in the way: a store without the table, or a write that
// fails, costs nothing but the log line. Contents are never logged — the tool
// name and a one-line summary say what happened, not what was in the file.
func (c *Client) Activity(agent, device, tool, summary string) {
	if !c.HasColumn("agent_activity", "tool") {
		return
	}
	if len(summary) > 200 {
		summary = summary[:200] + "…"
	}
	row, _ := json.Marshal([]map[string]string{{"agent": agent, "device": device, "tool": tool, "summary": summary}})
	_, _ = c.do(http.MethodPost, "agent_activity", row, "return=minimal")
}

// Tables lists the visible tables, sorted.
func (c *Client) Tables() ([]Table, error) {
	schema, err := c.Schema()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Table, 0, len(names))
	for _, n := range names {
		out = append(out, schema[n])
	}
	return out, nil
}

// HasColumn answers from the cached schema; unknown means false, never an error.
func (c *Client) HasColumn(table, column string) bool {
	schema, err := c.Schema()
	if err != nil {
		return false
	}
	for _, col := range schema[table].Columns {
		if col == column {
			return true
		}
	}
	return false
}

// Select runs a PostgREST query. filter is the raw query string PostgREST
// understands — `status=eq.open&order=created_at.desc&limit=20` — because
// inventing a second query language on top of one that already works well
// would only give the model a smaller vocabulary.
func (c *Client) Select(table, filter string, limit int) (json.RawMessage, error) {
	q, err := url.ParseQuery(filter)
	if err != nil {
		return nil, fmt.Errorf("filter is not a valid query string: %w", err)
	}
	if q.Get("limit") == "" {
		if limit <= 0 || limit > 200 {
			limit = 50
		}
		q.Set("limit", fmt.Sprint(limit))
	}
	return c.do(http.MethodGet, url.PathEscape(table)+"?"+q.Encode(), nil, "")
}

// Insert adds rows and returns them as stored — with the ids and defaults the
// database filled in, which is what the model needs to refer to them next.
func (c *Client) Insert(table string, rows json.RawMessage) (json.RawMessage, error) {
	return c.do(http.MethodPost, url.PathEscape(table), rows, "return=representation")
}

// Update patches every row the filter matches. A filter is required: PATCH
// without one would update the whole table, and no instruction to an agent
// should be one typo away from that.
func (c *Client) Update(table, filter string, patch json.RawMessage) (json.RawMessage, error) {
	if strings.TrimSpace(filter) == "" {
		return nil, fmt.Errorf("refusing to update %s without a filter", table)
	}
	q, err := url.ParseQuery(filter)
	if err != nil {
		return nil, fmt.Errorf("filter is not a valid query string: %w", err)
	}
	return c.do(http.MethodPatch, url.PathEscape(table)+"?"+q.Encode(), patch, "return=representation")
}

func summarizeError(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Details string `json:"details"`
		Hint    string `json:"hint"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		s := e.Message
		if e.Details != "" {
			s += " — " + e.Details
		}
		if e.Hint != "" {
			s += " (" + e.Hint + ")"
		}
		return s
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
