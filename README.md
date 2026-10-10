# ports

A small, dependency-free Go CLI that keeps track of host-port allocations for
Docker Compose stacks on a homelab server.

* Every stack under `/opt/stacks/<stack>` gets a predictable block of host
  ports (by default 5 ports from 12000–29999, first-fit).
* Services that need conventional ports (80/443, 8096, 53/udp, …) get fixed
  reservations in the same registry. Fixed ports are never moved.
* Reservations are scoped by protocol (TCP/UDP) and host bind address.
* State lives in a global registry plus a `.ports` manifest in each stack
  directory. Both are written with file locking, atomic renames and rollback.
* `ports check` and `ports audit` compare reservations and intended
  assignments with the effective Compose configuration (`docker compose
  config`) and with sockets that are listening on the host.

`ports` never edits Compose files, never starts or stops containers, and never
deletes stack data.

See [docs/allocation-model.md](docs/allocation-model.md) for the full
allocation model, conflict policy and write-ordering/recovery design, and
[examples/commands.md](examples/commands.md) for more example sessions,
including error cases.

## Build

Requires Go 1.24+ and Linux. Other Unix systems can build and run it, but host
socket inspection relies on `/proc/net`.

```sh
go build ./...          # compile all packages
go test ./...           # run unit and integration tests
make                    # vet + gofmt check + race tests + build bin/ports
make build              # just bin/ports (static, CGO disabled)
```

Without make: `go build -o bin/ports ./cmd/ports`.

Tests use only temporary directories. They need neither root nor Docker (a
fake `docker` script is used). `go test -short ./...` skips the test that
builds the binary and runs concurrent processes.

## Install and initialise a new server

```sh
make build
sudo install -m 0755 bin/ports /usr/local/bin/ports      # or: sudo make install

# Optional: only needed to change defaults
sudo install -d -m 0755 /etc/ports
sudo install -m 0644 examples/config.json /etc/ports/config.json

# Create /var/lib/ports and an empty registry (refuses to touch an existing one)
sudo ports init
```

`ports init` is the only command that creates the registry directory and
file. If the registry is missing, other commands fail with a clear message
instead of silently creating files.

Users who run mutating commands need write access to the registry directory
(for the lock file and atomic replacement) and to the stack directories.
Running as root works, and so does giving a group write access to
`/var/lib/ports` and `/opt/stacks`. Read-only commands only need read access.

If you already have stacks with `.ports` manifests (for example, after moving
to a new server), `sudo ports init --recover` rebuilds the registry from them.

## Quick start

```console
$ ports new jellyfin --fixed 8096
Registered stack jellyfin (/opt/stacks/jellyfin)
  reserved fixed 8096/tcp on 0.0.0.0
  reserved block 12000-12004/both on 0.0.0.0 (expansion)

$ ports assign jellyfin web 8096 --purpose "web UI"
Assigned web: 0.0.0.0:8096 -> 8096/tcp (web UI) in stack jellyfin
Compose port entry: "8096:8096"

$ ports jellyfin
Stack:      jellyfin
Directory:  /opt/stacks/jellyfin
Manifest:   /opt/stacks/jellyfin/.ports (ok)
Updated:    2026-10-10 18:51:25 UTC

Reservations:
  TYPE   PORTS        PROTO  BIND     PURPOSE
  fixed  8096         tcp    0.0.0.0  -
  block  12000-12004  both   0.0.0.0  expansion

Assignments (intended, not necessarily published):
  NAME  HOST          CONTAINER  PROTO  PURPOSE
  web   0.0.0.0:8096  8096       tcp    web UI

Compose port mappings (effective configuration):
  SERVICE   HOST          CONTAINER  PROTO
  jellyfin  0.0.0.0:8096  8096       tcp

$ ports check jellyfin
Stack jellyfin (/opt/stacks/jellyfin) - compose: ok, 1 published mapping(s)
  OK

Summary: 0 violation(s), 0 error(s), 0 warning(s), 0 info; host inspection: ok
Note: a clean audit does not guarantee ports are free at runtime; Docker's bind is authoritative.

$ ports list
$ ports find 8096
$ ports audit
```

The stack directory must already exist. `ports new` only creates it when you
pass `--init-dir`.

## Commands

| Command | Description |
|---------|-------------|
| `ports init [--recover]` | Create the registry. `--recover` rebuilds it from manifests. |
| `ports new <stack> [--fixed SPEC]... [--protocol P] [--bind ADDR] [--purpose T] [--block-size N \| --no-block] [--init-dir] [--compose-file F]...` | Register a stack with a dynamic block and/or fixed ports. |
| `ports <stack>` / `ports show <stack> [--no-compose]` | Show reservations, assignments, Compose mappings and discrepancies. Works without Docker. |
| `ports list` (`ls`) | List all stacks with their fixed ports and blocks. |
| `ports find <port> [--protocol tcp\|udp] [--bind ADDR]` | Show which stack reserves a port. |
| `ports assign <stack> <name> <container-port> [--host-port N] [--protocol P] [--bind ADDR] [--purpose T]` | Record an intended mapping. Picks the lowest free reserved port unless `--host-port` is given. |
| `ports reserve <stack> <SPEC> [--protocol P] [--bind ADDR] [--purpose T]` | Add fixed reservations to an existing stack. |
| `ports add-block <stack> [--block-size N]` | Allocate another dynamic block. |
| `ports release <stack> <name>` | Remove an assignment. The reservation is kept. |
| `ports release <stack> --fixed PORT [--protocol P] [--bind ADDR]` / `--block` | Remove a reservation (confirmation required). |
| `ports check <stack> [--compose-file F]... [--no-host] [--record]` | Audit one stack. |
| `ports audit [--no-host] [--record]` | Audit all stacks, plus registry-wide checks. |
| `ports sync [--dry-run \| --apply]` | Reconcile the registry and manifests (dry run by default). |
| `ports delete <stack> --release-ports` | Remove the registration (confirmation required; never deletes data). |
| `ports help [command]`, `ports version` | Help and version. |

`SPEC` is a comma-separated list of ports or ranges with an optional protocol
suffix: `80,443`, `8000-8005`, `53/udp`, `53/tcp,53/udp`. `--protocol` sets
the default for entries without a suffix.

Global options, accepted anywhere on the command line: `--config PATH`,
`--stack-root DIR`, `--registry PATH`, `--json`, `--yes`.

Destructive operations (`delete`, `release --fixed/--block`) ask you to type
`yes` on an interactive terminal. In scripts, pass `--yes`. Without a terminal
and without `--yes`, they fail.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success (audits: no violations or errors; warnings allowed) |
| 1 | General error: I/O, corrupt/unsupported files, confirmation declined |
| 2 | Usage error: bad arguments, invalid names or ports |
| 3 | Allocation conflict, exhausted range, or audit violations |
| 4 | Audit incomplete: Compose invalid, unresolved variables, Docker unavailable |
| 5 | Stack, assignment or port not found |
| 6 | Registry and manifest disagree; run `ports sync` |

With `--json`, results are printed as JSON on stdout. Errors are printed as
`{"error": "...", "exit_code": N}` on stdout and as text on stderr.

## Configuration

Precedence: built-in defaults < config file < environment < command-line flags.

| Setting | Config key | Env | Flag | Default |
|---------|-----------|-----|------|---------|
| Config file | – | `PORTS_CONFIG` | `--config` | `/etc/ports/config.json` |
| Stack root | `stack_root` | `PORTS_STACK_ROOT` | `--stack-root` | `/opt/stacks` |
| Registry | `registry_path` | `PORTS_REGISTRY` | `--registry` | `/var/lib/ports/registry.json` |
| Block size | `block_size` | | `--block-size` (per allocation) | `5` |
| Dynamic range | `dynamic_range.start/end` | | | `12000`–`29999` |
| Strategy | `strategy` | | | `first-fit` (only option) |
| Default Compose files | `compose_files` | | `--compose-file` | none (Compose defaults such as `compose.yml`) |
| Docker binary | `docker_command` | | | `docker` |
| Socket tables | `proc_net_dir` | | | `/proc/net` |

[`examples/config.json`](examples/config.json) lists every key with its
default value:

```json
{
  "version": 1,
  "stack_root": "/opt/stacks",
  "registry_path": "/var/lib/ports/registry.json",
  "block_size": 5,
  "dynamic_range": { "start": 12000, "end": 29999 },
  "strategy": "first-fit",
  "compose_files": [],
  "docker_command": "docker",
  "proc_net_dir": "/proc/net"
}
```

If the default config file is missing, the defaults are used. A config file
given explicitly with `--config` or `PORTS_CONFIG` must exist. Unknown keys,
malformed JSON and invalid values are reported, and the file is never
rewritten.

Compose files: relative paths are resolved inside the stack directory.
`ports new --compose-file` stores per-stack files in the registry, and
`ports check --compose-file` overrides them for one run. Otherwise
`compose_files` from the config is used, and if that is empty, Compose's own
defaults apply. Docker is invoked as `docker compose --project-directory
<stack> [-f F]... config --format json` from inside the stack directory, so
`.env` files and interpolation behave as they do in normal use.

## File formats

### Global registry: `/var/lib/ports/registry.json`

The authoritative record of ownership. See
[`examples/registry.json`](examples/registry.json).

```json
{
  "version": 1,
  "created_at": "2026-10-10T18:48:42Z",
  "updated_at": "2026-10-10T18:48:42Z",
  "stacks": {
    "jellyfin": {
      "name": "jellyfin",
      "directory": "/opt/stacks/jellyfin",
      "allocations": [
        { "type": "fixed", "start": 8096, "end": 8096, "protocol": "tcp", "bind_address": "0.0.0.0", "purpose": "web" },
        { "type": "block", "start": 12000, "end": 12004, "protocol": "both", "bind_address": "0.0.0.0", "purpose": "expansion" }
      ],
      "assignments": [
        { "name": "web", "host_port": 8096, "container_port": 8096, "protocol": "tcp", "bind_address": "0.0.0.0", "purpose": "web UI", "status": "assigned" }
      ],
      "created_at": "2026-10-10T18:48:42Z",
      "updated_at": "2026-10-10T18:48:42Z"
    }
  }
}
```

### Stack manifest: `/opt/stacks/<stack>/.ports`

A copy of the stack's registry entry, kept next to the stack so the allocation
travels with it and can be used to rebuild the registry. See
[`examples/stacks/jellyfin/.ports`](examples/stacks/jellyfin/.ports).

```json
{
  "version": 1,
  "stack": "jellyfin",
  "directory": "/opt/stacks/jellyfin",
  "allocations": [ "...same as the registry entry..." ],
  "assignments": [ "...same as the registry entry..." ],
  "updated_at": "2026-10-10T18:48:42Z",
  "observed": {
    "checked_at": "2026-10-10T19:00:00Z",
    "mappings": [
      { "service": "jellyfin", "host_port": 8096, "container_port": 8096, "protocol": "tcp", "bind_address": "0.0.0.0" }
    ]
  }
}
```

The manifest keeps three concepts apart:

* `allocations`: **reserved** ports (`fixed` or `block`).
* `assignments`: **intended** mappings, with status `assigned`. Being reserved
  or assigned never implies that a port is published or listening.
* `observed`: **actual** Compose mappings, written only by
  `check --record` or `audit --record`. This field is optional and is ignored
  when the manifest is compared with the registry.

Field defaults: `protocol` is `tcp` for fixed ports and assignments, and blocks
are always `both`. `bind_address` defaults to `0.0.0.0` (all addresses) and is
stored in canonical form. Both files carry a schema `version` (currently `1`).
Unknown versions are refused and never modified.

## Allocation model in brief

* Two reservations conflict when their port ranges overlap **and** their
  protocols overlap (`both` overlaps everything; TCP and UDP do not conflict)
  **and** their bind scopes overlap (the wildcards `0.0.0.0` and `::` overlap
  every address; distinct specific addresses do not).
* New blocks take the lowest free gap in the dynamic range, skipping every
  existing reservation and any port currently listening on the host.
* Unused ports in a block stay reserved for their stack.
* Assignments must be inside the stack's own reservations.

Full details: [docs/allocation-model.md](docs/allocation-model.md).

## Safety

* Registry access is serialised with `flock` on `registry.json.lock` (shared
  for reads, exclusive for writes, 30 s timeout). Concurrent `ports new`
  invocations cannot allocate the same block.
* Each file is written to a temp file in the same directory, fsynced and
  renamed into place. The manifest is written first, then the registry (the
  commit point). If the registry write fails, the manifest is restored. The two
  writes are not a single transaction: an interruption between them is detected
  (exit 6) and repaired with `ports sync --apply`.
* Every change is validated against the complete proposed registry before
  anything is written.
* Stack names are restricted to `[a-z0-9][a-z0-9_-]*`. Stack directories must
  be real directories directly under the stack root, and symlinked stack
  directories or manifests are refused (`O_NOFOLLOW`).
* Corrupt or unknown-version files are reported and never overwritten.
  Recovery paths keep copies (`.ports.bak-*`, `.ports.deleted-*`,
  `registry.json.corrupt-*`).
* No secrets are read or stored. Compose output is used only for port data,
  and only the relevant error lines are shown.

## Limitations

* The lock is advisory. Manual edits and other tools bypass it, though the
  resulting differences are caught as discrepancies.
* Host listeners are read from `/proc/net`. The owning process or container is
  not identified, and sockets in other network namespaces are not visible. A
  clean audit does not guarantee a port will be free when Docker binds it.
* A user with write access to the stack root could swap a directory between
  validation and rename. Keep the stack root writable only by trusted users.
* Version 1 does not edit Compose files. `ports assign` prints the Compose port
  entry for you to add by hand.
* The registry lock stays held while an interactive confirmation prompt is
  open.
