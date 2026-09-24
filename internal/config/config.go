// Package config is the agent's memory of who it is: which Norriva it belongs
// to, the session that proves it, and the folders it has been allowed into.
//
// One JSON file under ~/.norriva, mode 0600. It holds tokens, so it is never
// world-readable and never written anywhere but the user's own home.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Workspace is a local folder the agent may read and write. Nothing outside
// a linked workspace is ever touched; the path check lives in the tools.
type Workspace struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Model is how the agent reaches inference. Norriva hands this out at login:
// an OpenAI-compatible endpoint (a proxy in the Norriva project) and the key
// to use with it — normally the user's own session token, so one credential
// covers data and inference alike.
type Model struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Name    string `json:"name"`
}

type Config struct {
	// Host is the Norriva web app, where `norriva login` sends the browser.
	Host string `json:"host"`
	// SupabaseURL and AnonKey identify the Norriva project the data lives in.
	// Both come back from the login page, so a fresh install needs only Host.
	SupabaseURL string `json:"supabase_url"`
	AnonKey     string `json:"anon_key"`

	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Email        string `json:"email,omitempty"`

	Model      Model       `json:"model"`
	Workspaces []Workspace `json:"workspaces"`
}

// Dir is ~/.norriva, or $NORRIVA_HOME — the override exists so a demo and a
// real login can live side by side on one machine.
func Dir() string {
	if d := os.Getenv("NORRIVA_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".norriva"
	}
	return filepath.Join(home, ".norriva")
}

func Path() string { return filepath.Join(Dir(), "config.json") }

// Load returns an empty config when none exists yet; that is the state a fresh
// install is in, not an error.
func Load() (*Config, error) {
	var c Config
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return &c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", Path(), err)
	}
	return &c, nil
}

func (c *Config) Save() error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Write to a sibling and rename, so a crash mid-write cannot leave a
	// half-written file that fails to parse on the next run.
	tmp := Path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path())
}

func (c *Config) LoggedIn() bool { return c.AccessToken != "" && c.SupabaseURL != "" }

// Workspace finds a linked folder by name, or the only one when there is
// exactly one and no name was given.
func (c *Config) Workspace(name string) (*Workspace, error) {
	if name == "" {
		switch len(c.Workspaces) {
		case 0:
			return nil, errors.New("no folder is linked yet — run: norriva link <path>")
		case 1:
			return &c.Workspaces[0], nil
		default:
			return nil, fmt.Errorf("%d folders are linked; pick one with --workspace <name>", len(c.Workspaces))
		}
	}
	for i := range c.Workspaces {
		if c.Workspaces[i].Name == name {
			return &c.Workspaces[i], nil
		}
	}
	return nil, fmt.Errorf("no linked folder named %q", name)
}
