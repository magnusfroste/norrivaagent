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
	cfg  *config.Config
	http *http.Client
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

// Tables lists what the signed-in person can see, from PostgREST's own
// OpenAPI description. A project that hides the root behind an admin-only
// route (some gateways do) answers 403 here; the tools then fall back to
// asking for tables by name.
func (c *Client) Tables() ([]string, error) {
	req, _ := http.NewRequest(http.MethodGet, c.cfg.SupabaseURL+"/rest/v1/", nil)
	req.Header.Set("apikey", c.cfg.AnonKey)
	req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	req.Header.Set("Accept", "application/openapi+json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("the table list is not available to this session (%d)", res.StatusCode)
	}
	var spec struct {
		Definitions map[string]json.RawMessage `json:"definitions"`
	}
	if err := json.NewDecoder(res.Body).Decode(&spec); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(spec.Definitions))
	for name := range spec.Definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
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
