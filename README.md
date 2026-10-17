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

Requires Go 1.21+. The build has no external dependencies and produces a
single static binary:

```sh
make build          # CGO_ENABLED=0 go build -o snsync ./cmd/snsync
# or
CGO_ENABLED=0 go build -trimpath -o snsync ./cmd/snsync
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

# show state
./snsync status
```

Config lives at `~/.config/snsync/config.json` by default; use `-c path` to
point elsewhere.

```json
{
  "email": "you@example.com",
  "local_dir": "~/Notes",
  "app_id": "chalk-bump-f49",
  "app_key": "26864ab5d6fd4a37b80343439f107350"
}
```

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
| unchanged | deleted | move the local file to `.snsync/trash/` |
| changed | deleted | re-create it as a new note |

Deletions travel in both directions. Nothing is ever hard-deleted: local files
go to `.snsync/trash/`, remote notes get `deleted: true` (Simplenote's own
trash).

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

### Files and names

- One `.md` file per note. The file name is derived from the note title (the
  first non-empty line), so **renaming a file manually will be undone** on the
  next sync — change the note's first line instead. A manual rename that keeps
  the same content is detected by hash and adopted long enough not to create a
  duplicate note.
- Tags and the pinned/markdown system tags are **not** stored in the file, so
  local files stay pure note content. They live in the state file.
- Bookkeeping is kept in `<local_dir>/.snsync/state.json`; the trash is
  `<local_dir>/.snsync/trash/`.

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
