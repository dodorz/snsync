// Command snsync keeps a local markdown directory in two-way sync with a
// Simplenote account.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dodorz/snsync/internal/config"
	"github.com/dodorz/snsync/internal/lock"
	"github.com/dodorz/snsync/internal/simperium"
	"github.com/dodorz/snsync/internal/store"
	syncpkg "github.com/dodorz/snsync/internal/sync"
)

// version is the program version. It defaults to "dev" and is overridden at
// build time: -ldflags "-X main.version=$(git describe --tags --always)".
var version = "dev"

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		usage()
		os.Exit(2)
	}

	switch opts.command {
	case "":
		usage()
		os.Exit(2)
	case "init":
		err = runInit(opts.configPath)
	case "login":
		err = runLogin(opts.configPath, opts.token)
	case "sync":
		err = runSync(opts.configPath, opts.dryRun, opts.full, opts.allowMassDelete)
	case "watch":
		err = runWatch(opts.configPath, opts.dryRun, opts.full, opts.allowMassDelete, opts.interval, opts.intervalSet)
	case "status":
		err = runStatus(opts.configPath)
	case "purge-trash":
		err = runPurgeTrash(opts.configPath, opts.since, opts.yes, opts.jobs, opts.purgeOrphans)
	case "prune-state":
		err = runPruneState(opts.configPath, opts.dryRun)
	case "version":
		fmt.Printf("snsync %s\n", version)
	case "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", opts.command)
		usage()
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			// Another run (for example the previous cron tick) still owns the
			// state directory. Skipping is safe and keeps cron from piling up
			// processes; the next tick will catch up.
			fmt.Fprintln(os.Stderr, "snsync: another sync is already running; skipping")
			return
		}
		fatal(err)
	}
}

type options struct {
	command    string
	configPath string
	dryRun     bool
	full       bool
	// allowMassDelete overrides the safety check that stops a sync from
	// trashing most of the account when many local files disappeared.
	allowMassDelete bool
	token           string
	interval        time.Duration
	// intervalSet records whether --interval was given, so the config value can
	// be used when it was not.
	intervalSet bool
	// purge-trash options.
	since    time.Duration
	sinceSet bool
	yes      bool
	jobs     int
	// purgeOrphans also purges recently trashed notes whose content has no
	// surviving copy.
	purgeOrphans bool
}

func parseArgs(args []string) (*options, error) {
	o := &options{configPath: defaultConfigPath()}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", a)
			}
			i++
			o.configPath = args[i]
		case strings.HasPrefix(a, "-c="):
			o.configPath = strings.TrimPrefix(a, "-c=")
		case strings.HasPrefix(a, "--config="):
			o.configPath = strings.TrimPrefix(a, "--config=")
		case a == "--token" || a == "-token":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", a)
			}
			i++
			o.token = args[i]
		case strings.HasPrefix(a, "--token="):
			o.token = strings.TrimPrefix(a, "--token=")
		case a == "--interval" || a == "-interval":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", a)
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return nil, fmt.Errorf("invalid --interval: %w", err)
			}
			o.interval, o.intervalSet = d, true
		case strings.HasPrefix(a, "--interval="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--interval="))
			if err != nil {
				return nil, fmt.Errorf("invalid --interval: %w", err)
			}
			o.interval, o.intervalSet = d, true
		case a == "-dry-run" || a == "--dry-run":
			o.dryRun = true
		case a == "-full" || a == "--full":
			o.full = true
		case a == "-allow-mass-delete" || a == "--allow-mass-delete":
			o.allowMassDelete = true
		case a == "--since" || a == "-since":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", a)
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return nil, fmt.Errorf("invalid --since: %w", err)
			}
			o.since, o.sinceSet = d, true
		case strings.HasPrefix(a, "--since="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--since="))
			if err != nil {
				return nil, fmt.Errorf("invalid --since: %w", err)
			}
			o.since, o.sinceSet = d, true
		case a == "--yes" || a == "-yes":
			o.yes = true
		case a == "--purge-orphans" || a == "-purge-orphans":
			o.purgeOrphans = true
		case a == "--jobs" || a == "-jobs":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", a)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return nil, fmt.Errorf("invalid --jobs: %w", err)
			}
			o.jobs = n
		case strings.HasPrefix(a, "--jobs="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--jobs="))
			if err != nil {
				return nil, fmt.Errorf("invalid --jobs: %w", err)
			}
			o.jobs = n
		case a == "-h" || a == "--help":
			o.command = "help"
		case a == "-v" || a == "--version":
			o.command = "version"
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown flag %q", a)
		default:
			if o.command != "" {
				return nil, fmt.Errorf("unexpected argument %q", a)
			}
			o.command = a
		}
	}
	switch cmd := o.command; cmd {
	case "watch":
		if o.intervalSet && o.interval <= 0 {
			return nil, fmt.Errorf("--interval must be positive")
		}
	case "purge-trash":
		if !o.sinceSet || o.since <= 0 {
			return nil, fmt.Errorf("purge-trash needs a positive --since, e.g. --since 48h")
		}
		if o.jobs == 0 {
			o.jobs = 4
		}
		if o.jobs < 1 || o.jobs > 16 {
			return nil, fmt.Errorf("--jobs must be between 1 and 16")
		}
	}
	return o, nil
}

func runInit(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := `{
  "email": "you@example.com",
  "password": "",
  "local_dir": "~/Notes",
  "app_id": "` + config.DefaultAppID + `",
  "app_key": "` + config.DefaultAppKey + `"
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s\nEdit it, then run: snsync login -c %s\n", path, path)
	return nil
}

func runLogin(path, token string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// A token obtained elsewhere (e.g. the Simplenote web app cookie) can be
	// used directly, which avoids the often-blocked auth host entirely.
	if token != "" {
		norm, err := normalizeToken(token)
		if err != nil {
			return err
		}
		sn := newClient(cfg, norm)
		if err := sn.Ping(); err != nil {
			return fmt.Errorf("token rejected by %s: %w", sn.APIHost, err)
		}
		cfg.Token = norm
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("token verified and cached in %s\n", path)
		return nil
	}

	if cfg.Token != "" {
		sn := newClient(cfg, cfg.Token)
		if err := sn.Ping(); err == nil {
			fmt.Printf("cached token in %s is still valid\n", path)
			return nil
		}
	}

	if cfg.Password == "" {
		return fmt.Errorf("no password and no valid token.\n" + webTokenHint)
	}
	sn := newClient(cfg, "")
	tok, err := sn.Authorize(cfg.Email, cfg.Password)
	if err != nil {
		return fmt.Errorf("password login via %s failed: %w\n%s", sn.AuthHost, err, webTokenHint)
	}
	cfg.Token = tok
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("logged in as %s, token cached in %s\n", cfg.Email, path)
	return nil
}

const webTokenHint = `
If auth is unreachable, log in to the Simplenote web app and reuse its token:
  1. open https://app.simplenote.com and sign in
  2. browser DevTools > Application > Cookies > copy the "token" value
  3. run:  snsync login --token <value> -c <config>
     (or pipe it:  ... | snsync login --token - -c <config>)`

// normalizeToken trims whitespace/quotes and supports "-" for stdin so a
// token can be piped straight from a clipboard.
func normalizeToken(token string) (string, error) {
	if token == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		token = string(b)
	}
	token = strings.TrimSpace(token)
	token = strings.Trim(token, `"'`)
	if token == "" {
		return "", fmt.Errorf("empty token")
	}
	return token, nil
}

func newClient(cfg *config.Config, token string) *simperium.Client {
	sn := simperium.New(cfg.AppID, cfg.AppKey, token)
	if cfg.AuthHost != "" {
		sn.AuthHost = cfg.AuthHost
	}
	if cfg.APIHost != "" {
		sn.APIHost = cfg.APIHost
	}
	return sn
}

func runSync(path string, dry, full, allowMassDelete bool) error {
	cfg, sn, st, lk, err := loadLocked(path)
	if err != nil {
		return err
	}
	defer lk.Release()
	eng := syncpkg.New(cfg, sn, st)
	eng.Full = full
	eng.AllowMassDelete = allowMassDelete
	eng.Log = func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	}
	if err := eng.Run(dry); err != nil {
		return err
	}
	if dry {
		fmt.Println("done (dry run)")
	} else {
		fmt.Println("done")
	}
	return nil
}

func runWatch(path string, dry, full, allowMassDelete bool, interval time.Duration, intervalSet bool) error {
	cfg, sn, st, lk, err := loadLocked(path)
	if err != nil {
		return err
	}
	// A watch session owns the state directory for its whole lifetime, so a
	// cron-driven `sync` skips instead of racing it.
	defer lk.Release()
	if !intervalSet {
		if interval, err = cfg.WatchEvery(); err != nil {
			return err
		}
	}
	eng := syncpkg.New(cfg, sn, st)
	eng.AllowMassDelete = allowMassDelete
	eng.Log = func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("watching every %s (Ctrl-C to stop)\n", interval)
	first := true
	for {
		// Force a full sync on the first pass only; later passes use the
		// incremental cursor and the periodic full-sync counter.
		eng.Full = full && first
		if err := eng.Run(dry); err != nil {
			var authErr *simperium.AuthError
			if errors.As(err, &authErr) {
				return err
			}
			fmt.Fprintf(os.Stderr, "sync error: %v\n", err)
		} else {
			first = false
		}

		select {
		case <-ctx.Done():
			fmt.Println("stopped")
			return nil
		case <-time.After(interval):
		}
	}
}

func runStatus(path string) error {
	_, _, st, err := load(path)
	if err != nil {
		return err
	}
	var live, deleted, pending int
	for _, n := range st.Notes {
		if n.Deleted {
			deleted++
			continue
		}
		if n.ServerKey == "" {
			pending++
		}
		live++
	}
	fmt.Printf("state file:   %s\n", st.Path())
	fmt.Printf("cursor:       %s\n", st.Cursor)
	if st.LastFullSync > 0 {
		fmt.Printf("last full:    %s\n", time.Unix(int64(st.LastFullSync), 0).Format(time.RFC3339))
	}
	fmt.Printf("since full:   %d incremental sync(s)\n", st.IncrementalSinceFull)
	fmt.Printf("notes:        %d (%d deleted, %d not yet pushed)\n", len(st.Notes), deleted, pending)
	return nil
}

// loadLocked loads the config and state while holding the per-state-directory
// lock. The caller must release the returned lock when it is finished.
func loadLocked(path string) (*config.Config, *simperium.Client, *store.State, *lock.Lock, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, nil, nil, err
	}
	if cfg.StateFile == "" {
		return nil, nil, nil, nil, fmt.Errorf("local_dir is required to locate the state file")
	}
	lk, err := lock.Acquire(cfg.StateFile + ".lock")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	st, err := store.LoadState(cfg.StateFile)
	if err != nil {
		_ = lk.Release()
		return nil, nil, nil, nil, err
	}
	sn := newClient(cfg, cfg.Token)
	return cfg, sn, st, lk, nil
}

func load(path string) (*config.Config, *simperium.Client, *store.State, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, nil, err
	}
	if cfg.StateFile == "" {
		return nil, nil, nil, fmt.Errorf("local_dir is required to locate the state file")
	}
	st, err := store.LoadState(cfg.StateFile)
	if err != nil {
		return nil, nil, nil, err
	}
	sn := newClient(cfg, cfg.Token)
	return cfg, sn, st, nil
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "snsync.json"
	}
	return filepath.Join(dir, "snsync", "config.json")
}

func usage() {
	fmt.Fprint(os.Stderr, `snsync - two-way sync between a Simplenote account and a markdown directory

usage: snsync [flags] <command> [flags]

commands:
  init      write a starter config file
  login     authenticate and cache an access token
  sync      run one reconciliation pass
  watch     keep syncing on an interval until interrupted
  status    show sync state summary
  purge-trash
            permanently delete recently trashed notes (older trash is kept)
  prune-state
            drop bookkeeping for already-trashed notes to shrink state.json
  version   print the program version
  help      show this message

flags:
  -c, --config <path>   config file (default ~/.config/snsync/config.json)
      --dry-run         show what would happen without writing anything
      --full            force a full reconciliation (ignore the cursor)
      --allow-mass-delete
                        allow a sync to trash a large part of the account when
                        many tracked local files are missing (safety override)
      --since <dur>     purge-trash: only notes trashed within this window are
                        purged, e.g. 48h (required)
      --yes             purge-trash: actually delete (default is a dry run)
      --purge-orphans   purge-trash: also delete recent trash whose content has
                        no surviving live copy (dangerous; default keeps them)
      --jobs <n>        purge-trash: concurrent deletes, 1-16 (default 4)
      --interval <dur>  watch poll interval, e.g. 90s or 5m (default 60s or
                        the watch_interval config key)
      --token <value>   save a token obtained elsewhere (login only)
  -v, --version         print the program version
`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
