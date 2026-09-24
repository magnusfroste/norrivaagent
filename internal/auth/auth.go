// Package auth ties the binary on this machine to a person in Norriva.
//
// The flow is the one every good CLI uses: the agent opens the browser to the
// Norriva app, the person logs in there — with whatever Norriva uses, password,
// magic link, SSO — and the page hands the resulting session back to a
// listener on 127.0.0.1. The CLI never sees a password and never has to know
// how Norriva authenticates; it only has to be told who you turned out to be.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/magnusfroste/norrivaagent/internal/config"
)

// Callback is what the Norriva login page posts to the local listener once
// the person is signed in. Everything the agent needs to work arrives in one
// message: the session, where the data lives, and how to reach inference.
type Callback struct {
	State        string       `json:"state"`
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	SupabaseURL  string       `json:"supabase_url"`
	AnonKey      string       `json:"anon_key"`
	Model        config.Model `json:"model"`
}

const loginTimeout = 5 * time.Minute

// Login runs the browser flow and fills cfg in place. host is the Norriva app.
func Login(ctx context.Context, cfg *config.Config, host string, out io.Writer) error {
	host = strings.TrimRight(host, "/")
	if host == "" {
		return errors.New("which Norriva? pass --host https://your-norriva.app")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("could not open a local port for the login callback: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	state := randomToken(16)

	result := make(chan Callback, 1)
	srv := &http.Server{Handler: callbackHandler(state, result)}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	url := fmt.Sprintf("%s/cli-auth?port=%d&state=%s", host, port, state)
	fmt.Fprintf(out, "Opening %s\n", host)
	fmt.Fprintln(out, "If the browser does not open, visit:")
	fmt.Fprintf(out, "  %s\n\n", url)
	fmt.Fprintln(out, "Waiting for you to sign in…")
	openBrowser(url)

	select {
	case cb := <-result:
		cfg.Host = host
		cfg.SupabaseURL = strings.TrimRight(cb.SupabaseURL, "/")
		cfg.AnonKey = cb.AnonKey
		cfg.AccessToken = cb.AccessToken
		cfg.RefreshToken = cb.RefreshToken
		if cb.Model.BaseURL != "" {
			cfg.Model = cb.Model
		}
	case <-time.After(loginTimeout):
		return errors.New("no sign-in arrived within five minutes — run `norriva login` again")
	case <-ctx.Done():
		return ctx.Err()
	}

	email, err := WhoAmI(cfg)
	if err != nil {
		return fmt.Errorf("signed in, but Norriva would not say who you are: %w", err)
	}
	cfg.Email = email
	return nil
}

// LoginManual takes a session straight from the terminal — for development
// against a project that has no /cli-auth page yet, or for a script.
func LoginManual(cfg *config.Config, supabaseURL, anonKey, accessToken string) error {
	cfg.SupabaseURL = strings.TrimRight(supabaseURL, "/")
	cfg.AnonKey = anonKey
	cfg.AccessToken = accessToken
	cfg.RefreshToken = ""
	// A service-role key has no user behind it, so /auth/v1/user cannot answer
	// for it; the identity is the role itself. Real user tokens are checked.
	if role := jwtRole(accessToken); role != "" && role != "authenticated" {
		cfg.Email = role + " (not a person — development only)"
		return nil
	}
	email, err := WhoAmI(cfg)
	if err != nil {
		return err
	}
	cfg.Email = email
	return nil
}

func jwtRole(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.Role
}

func callbackHandler(state string, result chan<- Callback) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// The page posting to us lives on the Norriva origin; without these
		// headers the browser refuses to deliver its POST.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST the session here", http.StatusMethodNotAllowed)
			return
		}
		var cb Callback
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&cb); err != nil {
			http.Error(w, "that is not the session I expected", http.StatusBadRequest)
			return
		}
		// The state ties this callback to the login we started. Anything else
		// hitting the port — another tab, another program — is ignored.
		if cb.State != state {
			http.Error(w, "state does not match this login", http.StatusForbidden)
			return
		}
		if cb.AccessToken == "" || cb.SupabaseURL == "" {
			http.Error(w, "missing access_token or supabase_url", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Norriva</title>
<body style="font-family:system-ui;display:grid;place-items:center;height:100vh;margin:0;background:#0f172a;color:#e2e8f0">
<div style="text-align:center"><h1 style="font-weight:600">Signed in</h1><p>You can close this tab and go back to the terminal.</p></div>`)
		select {
		case result <- cb:
		default:
		}
	})
	return mux
}

// WhoAmI asks Norriva's auth service who this session belongs to.
func WhoAmI(cfg *config.Config) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, cfg.SupabaseURL+"/auth/v1/user", nil)
	req.Header.Set("apikey", cfg.AnonKey)
	req.Header.Set("Authorization", "Bearer "+cfg.AccessToken)
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("auth answered %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var u struct {
		Email string `json:"email"`
		ID    string `json:"id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&u); err != nil {
		return "", err
	}
	if u.Email == "" {
		return u.ID, nil
	}
	return u.Email, nil
}

// EnsureFresh refreshes the session when it is about to expire. Supabase
// access tokens last an hour; an agent left running past that would start
// getting 401s from the data it was just writing to.
func EnsureFresh(cfg *config.Config) error {
	exp, ok := jwtExpiry(cfg.AccessToken)
	if !ok || time.Until(exp) > 2*time.Minute {
		return nil
	}
	if cfg.RefreshToken == "" {
		return errors.New("session has expired and cannot be refreshed — run `norriva login`")
	}
	body := strings.NewReader(fmt.Sprintf(`{"refresh_token":%q}`, cfg.RefreshToken))
	req, _ := http.NewRequest(http.MethodPost, cfg.SupabaseURL+"/auth/v1/token?grant_type=refresh_token", body)
	req.Header.Set("apikey", cfg.AnonKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("session has expired — run `norriva login`")
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&t); err != nil {
		return err
	}
	cfg.AccessToken, cfg.RefreshToken = t.AccessToken, t.RefreshToken
	// When inference is reached with the same session, the model key follows.
	if cfg.Model.APIKey != "" && cfg.Model.APIKey == cfg.AccessToken {
		cfg.Model.APIKey = t.AccessToken
	}
	return cfg.Save()
}

// jwtExpiry reads exp out of a JWT without verifying it — we are the party
// that was issued the token, not the one checking it.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
