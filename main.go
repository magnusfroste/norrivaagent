// norriva — the agent on your own computer, signed in to Norriva.
//
// Install it, log in once, link a folder, and the agent can read and write
// that folder and the shared Norriva data as you. Or run `norriva mcp` and let
// the CLI agent you already use do the same.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/magnusfroste/norrivaagent/internal/agent"
	"github.com/magnusfroste/norrivaagent/internal/auth"
	"github.com/magnusfroste/norrivaagent/internal/config"
	"github.com/magnusfroste/norrivaagent/internal/mcp"
	"github.com/magnusfroste/norrivaagent/internal/watch"
)

// Set at build time: -ldflags "-X main.version=…"
var version = "dev"

// DefaultHost is where `norriva login` goes when no --host is given. Set at
// build time for a branded binary; the prototype points at the Lovable app.
var DefaultHost = ""

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "norriva:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	cmd, rest := args[0], args[1:]

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The model can always be overridden from the environment — for testing
	// against a plain OpenAI key before the Norriva proxy exists.
	if u := os.Getenv("NORRIVA_MODEL_URL"); u != "" {
		cfg.Model.BaseURL = u
	}
	if k := os.Getenv("NORRIVA_MODEL_KEY"); k != "" {
		cfg.Model.APIKey = k
	}
	if m := os.Getenv("NORRIVA_MODEL"); m != "" {
		cfg.Model.Name = m
	}

	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("norriva", version)
		return nil
	case "help", "--help", "-h":
		return usage()
	case "login":
		return cmdLogin(cfg, rest)
	case "logout":
		cfg.AccessToken, cfg.RefreshToken, cfg.Email = "", "", ""
		cfg.Model.APIKey = ""
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Println("Signed out. Linked folders are kept.")
		return nil
	case "status", "whoami":
		return cmdStatus(cfg)
	case "link":
		return cmdLink(cfg, rest)
	case "unlink":
		return cmdUnlink(cfg, rest)
	case "mcp":
		return cmdMCP(cfg, rest)
	case "chat":
		return cmdChat(cfg, rest)
	case "run":
		return cmdRun(cfg, rest)
	case "watch":
		return cmdWatch(cfg, rest)
	default:
		// `norriva "do this"` — the instruction is the whole command line.
		return cmdRun(cfg, args)
	}
}

func usage() error {
	fmt.Print(`norriva — the Norriva agent on your computer

  norriva login [--host URL]      sign in to Norriva in the browser
  norriva status                  who you are, what is linked, what model
  norriva link <path> [--name N]  let the agent work in a folder
  norriva unlink <name>           forget a folder
  norriva "<instruction>"         do one thing and report
  norriva chat                    a conversation, until you type exit
  norriva watch ["<instruction>"] drop a file in the folder; the agent acts on it
  norriva mcp [--workspace N]     serve the same tools over MCP (stdio)
  norriva logout

Environment (optional):
  NORRIVA_HOME        where config lives (default ~/.norriva)
  NORRIVA_MODEL_URL   OpenAI-compatible endpoint, overrides what login gave
  NORRIVA_MODEL_KEY   its key
  NORRIVA_MODEL       model name (default gpt-5.6-luna)
`)
	return nil
}

func cmdLogin(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	host := fs.String("host", firstNonEmpty(cfg.Host, DefaultHost), "the Norriva web app")
	manual := fs.Bool("manual", false, "paste a session instead of using the browser (development)")
	supa := fs.String("supabase", "", "with --manual: the Supabase URL")
	anon := fs.String("anon-key", "", "with --manual: the anon key")
	token := fs.String("token", "", "with --manual: an access token")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if *manual {
		if *supa == "" || *anon == "" || *token == "" {
			return fmt.Errorf("--manual needs --supabase, --anon-key and --token")
		}
		if err := auth.LoginManual(cfg, *supa, *anon, *token); err != nil {
			return err
		}
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if err := auth.Login(ctx, cfg, *host, os.Stdout); err != nil {
			return err
		}
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("\nSigned in as %s\n", cfg.Email)
	if cfg.Model.BaseURL != "" {
		fmt.Printf("Model: %s via %s\n", firstNonEmpty(cfg.Model.Name, "default"), cfg.Model.BaseURL)
	} else {
		fmt.Println("No model was handed out at login; set NORRIVA_MODEL_URL and NORRIVA_MODEL_KEY to run the agent.")
	}
	if len(cfg.Workspaces) == 0 {
		fmt.Println("Next: norriva link <folder>")
	}
	return nil
}

func cmdStatus(cfg *config.Config) error {
	fmt.Printf("norriva %s\n", version)
	fmt.Printf("config    %s\n", config.Path())
	if cfg.LoggedIn() {
		fmt.Printf("account   %s\n", cfg.Email)
		fmt.Printf("norriva   %s\n", firstNonEmpty(cfg.Host, cfg.SupabaseURL))
		if err := auth.EnsureFresh(cfg); err != nil {
			fmt.Printf("session   %s\n", err)
		} else {
			fmt.Println("session   valid")
		}
	} else {
		fmt.Println("account   not signed in — run: norriva login")
	}
	if cfg.Model.BaseURL != "" {
		fmt.Printf("model     %s via %s\n", firstNonEmpty(cfg.Model.Name, "default"), cfg.Model.BaseURL)
	} else {
		fmt.Println("model     none")
	}
	if len(cfg.Workspaces) == 0 {
		fmt.Println("folders   none linked — run: norriva link <path>")
	}
	for _, ws := range cfg.Workspaces {
		fmt.Printf("folder    %-12s %s\n", ws.Name, ws.Path)
	}
	return nil
}

func cmdLink(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	name := fs.String("name", "", "a short name for the folder (default: its basename)")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: norriva link <path> [--name N]")
	}
	abs, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a folder", abs)
	}
	n := *name
	if n == "" {
		n = filepath.Base(abs)
	}
	for i := range cfg.Workspaces {
		if cfg.Workspaces[i].Name == n {
			cfg.Workspaces[i].Path = abs
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("Updated %s → %s\n", n, abs)
			return nil
		}
	}
	cfg.Workspaces = append(cfg.Workspaces, config.Workspace{Name: n, Path: abs})
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("Linked %s → %s\nThe agent may now read and write inside it, and nowhere else.\n", n, abs)
	return nil
}

func cmdUnlink(cfg *config.Config, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: norriva unlink <name>")
	}
	kept := cfg.Workspaces[:0]
	found := false
	for _, ws := range cfg.Workspaces {
		if ws.Name == args[0] {
			found = true
			continue
		}
		kept = append(kept, ws)
	}
	if !found {
		return fmt.Errorf("no linked folder named %q", args[0])
	}
	cfg.Workspaces = kept
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("Unlinked %s\n", args[0])
	return nil
}

func workspaceFlag(fs *flag.FlagSet) *string {
	return fs.String("workspace", "", "which linked folder to use (needed only with several)")
}

func pickWorkspace(cfg *config.Config, name string) (*config.Workspace, error) {
	if name == "" && len(cfg.Workspaces) == 0 {
		return nil, nil // allowed: data-only
	}
	return cfg.Workspace(name)
}

func cmdMCP(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	wsName := workspaceFlag(fs)
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	ws, err := pickWorkspace(cfg, *wsName)
	if err != nil {
		return err
	}
	if cfg.LoggedIn() {
		_ = auth.EnsureFresh(cfg)
	}
	return mcp.Serve(cfg, ws, os.Stdin, os.Stdout, version)
}

func cmdRun(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	wsName := workspaceFlag(fs)
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		return fmt.Errorf("what should I do? norriva \"<instruction>\"")
	}
	ws, err := pickWorkspace(cfg, *wsName)
	if err != nil {
		return err
	}
	if cfg.LoggedIn() {
		if err := auth.EnsureFresh(cfg); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	_, err = agent.New(cfg, ws, os.Stdout).Run(ctx, prompt, nil)
	return err
}

// The drop box. Every file that lands in the linked folder is handed to the
// agent with this instruction, {file} replaced by the file's path. The
// default is deliberately general — the folder decides what the files are.
const defaultWatchPrompt = "A new file was just added to the linked folder: {file}. Work on that file only — the others in " +
	"the folder were handled when they arrived. Read it and put what it contains into Norriva, in every table " +
	"that fits: a bookkeeping export or result report fills the actual column of budget_lines (match item " +
	"name and month; update existing rows, never insert duplicates; a row whose actual is null is a gap to " +
	"fill, not a match to skip — after writing, list the rows still null for the months the file covers and " +
	"fill them). A meeting note becomes all of these: a " +
	"note (title and the gist), a customer for each company or person in it that is not already there, and an " +
	"opportunity for any deal it mentions, at the stage the note implies, with value and expected close if " +
	"given. Look at the tables and the existing rows first. Then say in one line what you did."

func cmdWatch(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	wsName := workspaceFlag(fs)
	every := fs.Duration("every", 2*time.Second, "how often to look")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	instruction := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if instruction == "" {
		instruction = defaultWatchPrompt
	}
	ws, err := pickWorkspace(cfg, *wsName)
	if err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("nothing to watch — link a folder first: norriva link <path>")
	}
	if !cfg.LoggedIn() {
		return fmt.Errorf("not signed in — run `norriva login` first")
	}
	// Fail now, not when the first file lands hours later with nobody watching.
	if err := auth.EnsureFresh(cfg); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Printf("Watching %s (%s). Drop a file in; Ctrl-C to stop.\n", ws.Name, ws.Path)
	return watch.Run(ctx, ws.Path, *every, func(rel string) {
		fmt.Printf("\n▸ new file: %s\n", rel)
		if err := auth.EnsureFresh(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "norriva:", err)
			return
		}
		prompt := strings.ReplaceAll(instruction, "{file}", rel)
		if _, err := agent.New(cfg, ws, os.Stdout).Run(ctx, prompt, nil); err != nil {
			fmt.Fprintln(os.Stderr, "norriva:", err)
		}
	})
}

func cmdChat(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	wsName := workspaceFlag(fs)
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	ws, err := pickWorkspace(cfg, *wsName)
	if err != nil {
		return err
	}
	if cfg.LoggedIn() {
		if err := auth.EnsureFresh(cfg); err != nil {
			return err
		}
	}
	a := agent.New(cfg, ws, os.Stdout)
	var history []agent.Message
	in := bufio.NewScanner(os.Stdin)
	fmt.Printf("norriva %s — %s. Type exit to leave.\n", version, firstNonEmpty(cfg.Email, "not signed in"))
	for {
		fmt.Print("\n❯ ")
		if !in.Scan() {
			fmt.Println()
			return nil
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		history, err = a.Run(ctx, line, history)
		stop()
		if err != nil {
			fmt.Fprintln(os.Stderr, "norriva:", err)
		}
	}
}

// flagsFirst reorders args so that flags precede positionals. Go's flag
// package stops at the first bare word, which makes `norriva link ./x --name y`
// fail with a usage error — the shape a person types most.
func flagsFirst(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// A value follows unless the flag was written as --name=value or is
			// a known boolean.
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !boolFlag(a) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func boolFlag(a string) bool {
	switch strings.TrimLeft(a, "-") {
	case "manual", "help", "h", "v", "version":
		return true
	}
	return false
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
