# snsync

Two-way sync between a **Simplenote** account and a local **markdown
directory**. Written in Go, no external dependencies.

```
snsync pull/push/delete
   local dir  <──────────────>  Simplenote (Simperium)
```

## Why

Simplenote is a great minimal cross-device scratchpad, but its public API is
gone. This tool reuses the community-maintained Simperium credentials that
sncli ships (the only ones explicitly shared for third-party use) and presents
the account as a plain directory of `.md` files, so any editor, script or
version-control workflow can read and write it.

## Install

### Download a release

Prebuilt binaries for Linux and macOS (amd64 + arm64) are attached to every
[release](https://github.com/dodorz/snsync/releases), and Windows builds ship
as a `.7z` archive containing `snsync.exe` and `README.md`:

```sh
# Linux / macOS
VERSION=0.1.9
curl -L -o snsync "https://github.com/dodorz/snsync/releases/download/$VERSION/snsync-$VERSION-linux-amd64"
chmod +x snsync
```

```powershell
# Windows
$VERSION = '0.1.9'
curl.exe -L -o snsync.7z "https://github.com/dodorz/snsync/releases/download/$VERSION/snsync-$VERSION-windows-amd64.7z"
7z x snsync.7z
```

### With Go

```sh
go install github.com/dodorz/snsync/cmd/snsync@latest
```

`go install` builds without a version number, so `snsync version` prints `dev`.
Use the release binary or `make build` if you want a real version string.

### From source

Requires Go 1.21+. No external dependencies; produces a single static binary:

```sh
make build          # injects the version from git describe
# or, without a version number (prints "dev"):
CGO_ENABLED=0 go build -trimpath -o snsync ./cmd/snsync
```

### Version

The version is a single variable in `cmd/snsync/main.go`:

```go
var version = "dev"
```

`make build` overwrites it at link time from `git describe --tags --always
--dirty`, so tagged releases report e.g. `0.1.0` and untagged builds report
the commit hash. Check it with `snsync version` or `snsync --version`.

### Releases

Pushing a version tag (e.g. `0.1.1`) runs
`.github/workflows/release.yml`: it lints/tests, cross-compiles static
binaries for Linux, macOS and Windows (amd64 + arm64), writes a
`checksums.txt`, and attaches everything to the GitHub Release for that tag.

```sh
git tag 0.1.9 && git push origin 0.1.9
```

## Usage

```sh
# 1. create a starter config
./snsync init

# edit it: set email + local_dir, then authenticate once
./snsync login

# 2. sync (run it as often as you like; it is idempotent)
./snsync sync

# preview without touching anything
./snsync sync -dry-run

# force a full reconciliation (ignore the incremental cursor)
./snsync sync --full

# override the mass-deletion guard after intentionally emptying the folder
./snsync sync --allow-mass-delete

# keep syncing until interrupted (polls every 60s by default)
./snsync watch
./snsync watch --interval 90s

# show state
./snsync status

# rename local files to the sync naming rule (front matter title / first line)
./snsync rename --dry-run
./snsync rename

# permanently delete notes trashed in the last 48h (dry run first)
./snsync purge-trash --since 48h
./snsync purge-trash --since 48h --yes

# drop bookkeeping for notes that are already in the trash
./snsync prune-state

# restore pins removed by 0.1.9 (dry run first; reads server version history)
./snsync recover-pins
./snsync recover-pins --yes
```

Config lives at `~/.config/snsync/config.json` by default; use `-c path` to
point elsewhere.

```json
{
  "email": "you@example.com",
  "local_dir": "~/Notes",
  "app_id": "chalk-bump-f49",
  "app_key": "26864ab5d6fd4a37b80343439f107350",
  "full_sync_every": 10,
  "watch_interval": "90s"
}
```

All keys except `email` and `local_dir` are optional:

| key | default | meaning |
|---|---|---|
| `app_id`, `app_key` | the shared community credentials | Simperium app identity |
| `full_sync_every` | `10` | force a full reconciliation after this many incremental syncs (`0` disables) |
| `watch_interval` | `60s` | default `watch` poll interval, as a duration (`"90s"`, `"5m"`) or a number of seconds (`300`); `--interval` overrides it |
| `auth_host`, `api_host` | `https://auth.simperium.com`, `https://api.simperium.com` | override the Simperium hosts, e.g. when one is blocked |
| `state_file` | `<local_dir>/.snsync/state.json` | where sync bookkeeping lives |
| `trash_dir` | `<local_dir>/.trash` | where locally deleted notes go |

On Windows, JSON strings treat `\` as an escape character, so write paths with
forward slashes (Go accepts them everywhere) or double every backslash. The
drive-letter colon is literal and needs no escaping:

```json
{ "local_dir": "C:/Users/you/Notes" }
{ "local_dir": "C:\\Users\\you\\Notes" }
```

A `~` is expanded to `%USERPROFILE%` only when it is the first component of the
path: `~`, `~/Notes` and `~\Notes` all expand. A `~` anywhere else is an
ordinary directory name (legal on Windows), so `C:/~/Temp` refers to a folder
literally named `~` and is used as-is. The default config file is
`%AppData%\snsync\config.json`.

After `login` an access token is cached in the same file. The password is never
required again (and never stored unless you put it there). You can also supply
`SN_EMAIL`, `SN_PASSWORD` and `SN_TOKEN` via the environment.

## How the sync works

Each note is compared three ways: the last-synced snapshot (`base`), the
current server state, and the current local file.

| local vs base | server vs base | action |
|---|---|---|
| unchanged | unchanged | nothing |
| changed | unchanged | push |
| unchanged | changed | pull |
| changed | changed, equal | converge |
| changed | changed, different | **conflict**: pull remote, save local as `… (conflict).md` and push it as a new note |
| deleted | unchanged | move remote note to the trash |
| deleted | changed | restore the remote version |
| unchanged | deleted | move the local file to `.trash/` |
| changed | deleted | re-create it as a new note |

Deletions travel in both directions. Nothing is ever hard-deleted: local files
go to `.trash/`, remote notes get `deleted: true` (Simplenote's own
trash). A local deletion is logged as `trash  <file> (remote)`.

When the locally deleted note is pinned, the deletion is propagated in two
remote writes: first the pin is removed (`unpin  <file> (before remote trash)`),
and only after that succeeds is the note trashed. A failed trash therefore
still leaves the unpin recorded, and the next run finishes the deletion.

Because a missing local file normally means "delete it remotely", a state file
that no longer matches the directory (for example after the directory was moved
or restored from a backup) used to look like a deliberate mass deletion. The
engine now refuses to propagate **20 or more** local deletions that make up at
least **25%** of the tracked notes, before anything is written. Restore the
files if they are still wanted, or re-run with `--allow-mass-delete` for an
intentional cleanup.

### Emptying recent trash

`snsync purge-trash --since 48h` permanently deletes notes that were moved to
the Simplenote trash within the last 48 hours and leaves older trash alone. It
normally uses the deletion times recorded in the state file; when the state has
no tombstones left (for example after `prune-state`), or when `--remote` is
given, it scans the full remote index instead, which can take a while on a
large account but also sees notes trashed by another client. The command is a
dry run unless `--yes` is given:

```sh
./snsync purge-trash --since 48h              # show how many would be deleted
./snsync purge-trash --since 48h --yes        # delete them for real
./snsync purge-trash --since 48h --yes --jobs 8
```

By default a recently trashed note is only purged when its content still
exists in a live note, so the cleanup cannot permanently remove the only
remaining copy of something. The dry run reports how many such "orphans" were
kept; `--purge-orphans` deletes those as well.

Run a successful `sync` first so local changes are already reflected remotely.
Hard deletes cannot be undone; a note deleted this way no longer appears in
Simplenote's trash. Notes already absent from the server (HTTP 404) are treated
as successfully purged, so an interrupted cleanup can simply be re-run.

`state.json` only needs entries for **live** notes: they map a local file to a
server key and hold the last synced hash. Trashed notes are recognised as
deleted from the remote index, so full syncs no longer copy the whole
Simplenote trash into the state file. An older or already bloated state file
can be compacted with `snsync prune-state`, which drops every `deleted` entry
and leaves the cursor and live notes untouched. Add `--dry-run` to preview it.

### Incremental sync

After the first run the engine remembers Simperium's index cursor (`current`)
and only fetches notes changed since then. Because a hard-deleted note simply
vanishes from incremental data, a **full reconciliation runs every
`full_sync_every` incremental syncs** (default 10; set `0` to disable, which
means full syncs only happen on the first run and with `--full`).

In incremental mode a tracked note absent from the response is assumed
unchanged rather than missing, so nothing is deleted locally by omission. If a
local file was removed, the note is fetched once before being trashed so its
body is preserved.

A full index page can be tens of megabytes, and Simperium sometimes drops a
large response mid-transfer. Idempotent requests are retried a few times with
backoff on network errors, truncated bodies, `5xx` and `429`; a request
canceled by the user is not retried. Before this, a cut-off response was fed
to the JSON parser and surfaced as a misleading `bad JSON: invalid character
' ' in string escape code` error that aborted the sync.

If the state file is lost (for example because a run was killed before it could
save), the next full sync matches untracked local files to untracked remote
notes by content. The existing note is adopted instead of being pushed again,
so a lost state file cannot turn into an endless stream of duplicate notes.

### Watch mode and cron

`snsync watch` runs a sync, waits `--interval` (or the `watch_interval` config
key, default 60s), then repeats until interrupted with Ctrl-C. The first pass is a full reconciliation (or
forced full with `--full`); later passes use the incremental cursor and the
periodic full-sync counter. Transient errors (network) are logged and retried
on the next tick; an authentication error stops the loop.

A single HTTP request gets five minutes, which must cover downloading an
entire index page; requests are retried (see above) before a sync is declared
failed.

Every `sync` and `watch` run takes an exclusive lock on the state file
(`<local_dir>/.snsync/state.json.lock`). A second invocation that starts while
the first is still running — the usual case when a cron job fires every 10
minutes but a large sync takes longer — prints `another sync is already
running; skipping`, exits successfully, and lets the next tick catch up. This
prevents two processes from racing on the same state, which used to create
duplicate local files and duplicate Simplenote notes. `watch` holds the lock
for its whole lifetime, so a cron-driven `sync` skips while `watch` is active.
The lock is an OS file lock and is released automatically if a process crashes
or is killed; the lock file itself can be left on disk.

### Files and names

- One `.md` file per note. The file name is derived from the note title — its
  front matter's `title` when the file declares one, otherwise the body's first
  non-empty line — so a **manual rename is still undone** on the next sync:
  change one of those two instead. What a rename no longer does is split the
  note in two. The file it moved is matched back to its note in three steps, in
  decreasing order of confidence: by the `simplenote_id` in its front matter, by
  an identical body, and — when exactly one file and one note are left
  unaccounted for — by a plain one-to-one pairing that also covers a rename
  combined with an edit in the same step. Only that last step is a guess, so the
  body it overwrites is copied into `.trash/` first. When the guess is not safe
  to make (several notes lost their file at once) nothing is invented: the
  missing notes go to Simplenote's trash and the new file becomes a new note,
  exactly as before. Run `snsync rename` to apply the naming rule to existing
  files without contacting the server (`--dry-run` previews it); it also updates
  the state file, so the next sync sees no renames.
- `snsync` records which Simplenote note each file belongs to in the front
  matter:

  ```yaml
  ---
  simplenote_id: 4d5e6f...
  ---
  ```

  Like the rest of the front matter this is **local bookkeeping and is never
  uploaded**; Simplenote itself knows nothing about file names. The value is
  rewritten from the state file on every pass, so a hand-edited or copied id is
  repaired rather than trusted, and copying a file gives the copy its own note
  instead of hijacking the original. It is YAML-only: files with `+++` (TOML)
  front matter are left alone and fall back to the matching above. The first
  sync after upgrading rewrites each file once to add the id.
- Tags are stored in the front matter's `tags` field, one per line:

  ```yaml
  tags:
    - work
    - home
  ```

  An inline list (`tags: [work, home]`) is also accepted when reading. Editing
  them in the file pushes the new set to Simplenote, and remote changes are
  written back.
- Pinning is mirrored as `pinned: true` in the front matter:

  ```yaml
  pinned: true
  ```

  A missing key means `false`, so deleting the line (or setting it to `false`)
  unpins on the next sync. This is YAML-only: files with `+++` (TOML) front
  matter keep the server's pin. The `markdown` system tag is still kept only in
  the state file.
- A file that declares its own `title` keeps that name:

  ```yaml
  ---
  title: Meeting notes
  ---
  first line of the body
  ```

  becomes `Meeting notes.md`, and renaming it or editing `title:` renames the
  file (locally only — like everything in the front matter, the title is never
  uploaded and Simplenote keeps showing the note's first line). Remove the key
  and the name follows the body again. YAML only, as with the id.
- Front matter — a leading `---` (YAML) or `+++` (TOML) block — stays on disk.
  Apart from `title`, `tags`, `pinned` and `simplenote_id`, it is ignored when
  comparing, hashing and uploading, and is preserved when the file is updated
  from the server, so it never appears in Simplenote. Only `tags` or `pinned`
  changing triggers an upload; renaming the file or editing `title` or any other
  front matter key is invisible to the server.
- Local markdown files are written with **CRLF** (`\r\n`) line endings. Any
  line-ending style is accepted when reading, and existing files are converted
  on the next sync.
- Bookkeeping is kept in `<local_dir>/.snsync/state.json`; the trash is
  `<local_dir>/.trash/`. A `state.json.lock` file next to the state file is used
  to stop concurrent runs; it can be ignored and is never edited by hand.

## When the auth host is unreachable (e.g. blocked networks)

Simplenote's data lives on `api.simperium.com`; login normally goes through
`auth.simperium.com`. On some networks only the latter is blocked. Two things
help:

- **Reuse the web app's token.** Log in at <https://app.simplenote.com>,
  copy the `token` cookie (DevTools → Application → Cookies), then:

  ```sh
  ./snsync login --token <value> -c ~/.config/snsync/config.json
  # or pipe it from the clipboard:
  pbpaste | ./snsync login --token - -c ~/.config/snsync/config.json
  ```

  No password or `auth.simperium.com` needed; the token is verified against
  `api.simperium.com` before being cached.
- **Use a proxy.** The client honors `HTTPS_PROXY`/`HTTP_PROXY`. You can also
  override the hosts via `auth_host`/`api_host` in the config or the
  `SN_AUTH_HOST`/`SN_API_HOST` env vars.

The HTTP client is pinned to HTTP/1.1 on purpose: Simperium's nginx edge
answers Go's HTTP/2 requests with a spurious `403 Forbidden`.

## Caveats

- Uses unofficial, shared credentials. Be polite: sync on demand, do not poll
  aggressively. If Automattic revokes the key, everything stops working.
- This is a personal-scale tool, not a hosted service.

## Development

```sh
go test ./...
go vet ./...
```

The sync engine is tested against an in-memory fake Simperium server covering
push, pull, both deletion directions, conflicts, idempotency and renames.
