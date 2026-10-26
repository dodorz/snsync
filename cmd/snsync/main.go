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
		err = runLoginProfiles(opts.configPath, opts.profiles, opts.token)
	case "sync":
		err = forEachProfile(opts.configPath, opts.profiles, func(p *config.Profile) error {
			return runSyncProfile(p, opts.dryRun, opts.full, opts.allowMassDelete)
		})
	case "watch":
		err = runWatchProfiles(opts.configPath, opts.profiles, opts.dryRun, opts.full, opts.allowMassDelete, opts.interval, opts.intervalSet)
	case "status":
		err = forEachProfile(opts.configPath, opts.profiles, func(p *config.Profile) error {
			return runStatusProfile(p)
		})
	case "rename":
		err = forEachProfile(opts.configPath, opts.profiles, func(p *config.Profile) error {
			return runRenameProfile(p, opts.dryRun)
		})
	case "purge-trash":
		err = forEachProfile(opts.configPath, opts.profiles, func(p *config.Profile) error {
			return runPurgeTrashProfile(p, opts.since, opts.yes, opts.jobs, opts.purgeOrphans, opts.purgeRemote)
		})
	case "prune-state":
		err = forEachProfile(opts.configPath, opts.profiles, func(p *config.Profile) error {
			return runPruneStateProfile(p, opts.dryRun)
		})
	case "recover-pins":
		err = forEachProfile(opts.configPath, opts.profiles, func(p *config.Profile) error {
			return runRecoverPinsProfile(p, opts.yes)
		})
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
	// purgeRemote scans the full remote index instead of the state tombstones.
	purgeRemote bool
	// profiles selects config sections; empty means every profile.
	profiles []string
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
		case a == "--profile" || a == "-profile":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", a)
			}
			i++
			names := splitProfiles(args[i])
			if len(names) == 0 {
				return nil, fmt.Errorf("--profile needs a name")
			}
			o.profiles = append(o.profiles, names...)
		case strings.HasPrefix(a, "--profile="):
			names := splitProfiles(strings.TrimPrefix(a, "--profile="))
			if len(names) == 0 {
				return nil, fmt.Errorf("--profile needs a name")
			}
			o.profiles = append(o.profiles, names...)
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
		case a == "--remote" || a == "-remote":
			o.purgeRemote = true
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
  "profiles": {
    "default": {
      "email": "you@example.com",
      "password": "",
      "local_dir": "~/Notes",
      "app_id": "` + config.DefaultAppID + `",
      "app_key": "` + config.DefaultAppKey + `"
    }
  }
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s\nEdit it, then run: snsync login -c %s\n", path, path)
	return nil
}

// runLoginProfiles resolves a stdin token once and then logs in every selected
// profile.
func runLoginProfiles(path string, names []string, token string) error {
	if token != "" {
		norm, err := normalizeToken(token)
		if err != nil {
			return err
		}
		token = norm
	}
	return forEachProfile(path, names, func(p *config.Profile) error {
		return runLoginProfile(p, token)
	})
}

func runLoginProfile(p *config.Profile, token string) error {
	cfg := p.Config
	if err := cfg.Validate(); err != nil {
		return err
	}

	// A token obtained elsewhere (e.g. the Simplenote web app cookie) can be
	// used directly, which avoids the often-blocked auth host entirely.
	if token != "" {
		sn := newClient(cfg, token)
		if err := sn.Ping(); err != nil {
			return fmt.Errorf("token rejected by %s: %w", sn.APIHost, err)
		}
		cfg.Token = token
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("token verified and cached for %s in %s\n", p.Name, cfg.Path())
		return nil
	}

	if cfg.Token != "" {
		sn := newClient(cfg, cfg.Token)
		if err := sn.Ping(); err == nil {
			fmt.Printf("cached token for %s in %s is still valid\n", p.Name, cfg.Path())
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
	fmt.Printf("logged in as %s (%s), token cached in %s\n", cfg.Email, p.Name, cfg.Path())
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

func runSyncProfile(p *config.Profile, dry, full, allowMassDelete bool) error {
	cfg, sn, st, lk, err := loadProfileLocked(p)
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

type watchRunner struct {
	profile *config.Profile
	cfg     *config.Config
	eng     *syncpkg.Engine
	lk      *lock.Lock
	first   bool
}

func runWatchProfiles(path string, names []string, dry, full, allowMassDelete bool, interval time.Duration, intervalSet bool) error {
	ps, err := selectProfiles(path, names)
	if err != nil {
		return err
	}
	runners := make([]*watchRunner, 0, len(ps))
	release := func() {
		for _, r := range runners {
			_ = r.lk.Release()
		}
	}
	for _, p := range ps {
		cfg, sn, st, lk, err := loadProfileLocked(p)
		if err != nil {
			release()
			return err
		}
		eng := syncpkg.New(cfg, sn, st)
		eng.AllowMassDelete = allowMassDelete
		if len(ps) > 1 {
			name := p.Name
			eng.Log = func(format string, args ...any) {
				fmt.Printf("["+name+"] "+format+"\n", args...)
			}
		} else {
			eng.Log = func(format string, args ...any) {
				fmt.Printf(format+"\n", args...)
			}
		}
		runners = append(runners, &watchRunner{profile: p, cfg: cfg, eng: eng, lk: lk, first: true})
	}
	// A watch session owns the state directories for its whole lifetime, so a
	// cron-driven `sync` skips instead of racing it.
	defer release()

	if !intervalSet {
		if interval, err = watchInterval(runners); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("watching %d profile(s) every %s (Ctrl-C to stop)\n", len(runners), interval)
	for {
		for _, r := range runners {
			// Force a full sync on the first pass only; later passes use the
			// incremental cursor and the periodic full-sync counter.
			r.eng.Full = full && r.first
			if err := r.eng.Run(dry); err != nil {
				var authErr *simperium.AuthError
				if errors.As(err, &authErr) {
					if len(runners) > 1 {
						return fmt.Errorf("%s: %w", r.profile.Name, err)
					}
					return err
				}
				fmt.Fprintf(os.Stderr, "[%s] sync error: %v\n", r.profile.Name, err)
			} else {
				r.first = false
			}
		}

		select {
		case <-ctx.Done():
			fmt.Println("stopped")
			return nil
		case <-time.After(interval):
		}
	}
}

// watchInterval uses the smallest configured interval so no profile is polled
// less often than it asked for.
func watchInterval(runners []*watchRunner) (time.Duration, error) {
	best := time.Duration(0)
	for i, r := range runners {
		d, err := r.cfg.WatchEvery()
		if err != nil {
			return 0, fmt.Errorf("%s: %w", r.profile.Name, err)
		}
		if i == 0 || d < best {
			best = d
		}
	}
	return best, nil
}

func runStatusProfile(p *config.Profile) error {
	_, _, st, err := loadProfile(p)
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

// runRename renames local files to match the sync naming rule (front matter
// "title", falling back to the body's first line) without touching the server.
func runRenameProfile(p *config.Profile, dryRun bool) error {
	cfg := p.Config
	if cfg.LocalDir == "" {
		return fmt.Errorf("config: local_dir is required")
	}
	if !dryRun {
		if cfg.StateFile == "" {
			return fmt.Errorf("local_dir is required to locate the state file")
		}
		lk, err := lock.Acquire(cfg.StateFile + ".lock")
		if err != nil {
			return err
		}
		defer lk.Release()
	}

	renames, err := syncpkg.PlanLocalRenames(cfg.LocalDir)
	if err != nil {
		return err
	}
	if len(renames) == 0 {
		fmt.Println("no renames needed")
		return nil
	}
	for _, r := range renames {
		fmt.Printf("rename %s -> %s\n", r.From, r.To)
	}
	if dryRun {
		fmt.Printf("dry run: %d file(s) would be renamed\n", len(renames))
		return nil
	}
	if err := syncpkg.ApplyLocalRenames(cfg.LocalDir, renames); err != nil {
		return err
	}
	if err := updateStateFilenames(cfg.StateFile, renames); err != nil {
		return err
	}
	fmt.Printf("renamed %d file(s)\n", len(renames))
	return nil
}

// updateStateFilenames keeps the state file in step with renamed local files,
// so the next sync finds them without going through rename detection.
func updateStateFilenames(path string, renames []syncpkg.Rename) error {
	if path == "" {
		return nil
	}
	st, err := store.LoadState(path)
	if err != nil {
		return err
	}
	if len(st.Notes) == 0 {
		return nil
	}
	byOld := make(map[string]string, len(renames))
	for _, r := range renames {
		byOld[r.From] = r.To
	}
	changed := false
	for _, ns := range st.Notes {
		if to, ok := byOld[ns.Filename]; ok {
			ns.Filename = to
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return st.Save()
}

// selectProfiles loads the config file and returns the requested profiles, or
// all of them when names is empty.
func selectProfiles(path string, names []string) ([]*config.Profile, error) {
	all, err := config.LoadProfiles(path)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		if err := checkProfileStates(all); err != nil {
			return nil, err
		}
		return all, nil
	}
	byName := make(map[string]*config.Profile, len(all))
	for _, p := range all {
		byName[p.Name] = p
	}
	var out []*config.Profile
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		p, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown profile %q (available: %s)", name, strings.Join(profileNames(all), ", "))
		}
		out = append(out, p)
	}
	if err := checkProfileStates(out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkProfileStates rejects two selected profiles that share a state file:
// they would overwrite each other's bookkeeping and fight over the lock.
func checkProfileStates(ps []*config.Profile) error {
	seen := make(map[string]string, len(ps))
	for _, p := range ps {
		if p.Config.StateFile == "" {
			continue
		}
		key := filepath.Clean(p.Config.StateFile)
		if other, ok := seen[key]; ok {
			return fmt.Errorf("profiles %q and %q use the same state file %s", other, p.Name, key)
		}
		seen[key] = p.Name
	}
	return nil
}

func profileNames(ps []*config.Profile) []string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return names
}

// forEachProfile runs fn for each selected profile, with a short header when
// more than one profile is active.
func forEachProfile(path string, names []string, fn func(*config.Profile) error) error {
	ps, err := selectProfiles(path, names)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if len(ps) > 1 {
			fmt.Printf("== %s ==\n", p.Name)
		}
		if err := fn(p); err != nil {
			if len(ps) > 1 {
				return fmt.Errorf("%s: %w", p.Name, err)
			}
			return err
		}
	}
	return nil
}

func splitProfiles(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadProfileLocked loads a profile and its state while holding the
// per-state-directory lock. The caller must release the returned lock.
func loadProfileLocked(p *config.Profile) (*config.Config, *simperium.Client, *store.State, *lock.Lock, error) {
	cfg := p.Config
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

func loadProfile(p *config.Profile) (*config.Config, *simperium.Client, *store.State, error) {
	cfg := p.Config
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
  rename    rename local files to match the sync naming rule (no server)
  purge-trash
            permanently delete recently trashed notes (older trash is kept)
  prune-state
            drop bookkeeping for already-trashed notes to shrink state.json
  recover-pins
            restore pins lost by 0.1.9 from the previous server version
  version   print the program version
  help      show this message

flags:
  -c, --config <path>   config file (default ~/.config/snsync/config.json)
      --profile <name>  config profile/section to use; repeatable or
                        comma-separated (default: all profiles)
      --dry-run         show what would happen without writing anything
      --full            force a full reconciliation (ignore the cursor)
      --allow-mass-delete
                        allow a sync to trash a large part of the account when
                        many tracked local files are missing (safety override)
      --since <dur>     purge-trash: only notes trashed within this window are
                        purged, e.g. 48h (required)
      --yes             purge-trash / recover-pins: actually apply (default is a dry run)
      --purge-orphans   purge-trash: also delete recent trash whose content has
                        no surviving live copy (dangerous; default keeps them)
      --remote          purge-trash: scan the full remote index instead of the
                        state-file tombstones (slower, sees all clients)
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
