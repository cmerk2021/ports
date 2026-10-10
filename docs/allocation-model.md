# Allocation model and conflict policy

This document is the reference for how `ports` decides who owns a host port,
when two reservations conflict, how audits are classified, and how state is
written safely.

## Terms

| Term | Meaning |
|------|---------|
| **Reservation** (allocation) | A range of host ports owned by one stack. Reserved ≠ listening ≠ available. |
| **Fixed** reservation | Ports chosen by the operator (e.g. 80, 443, 8096). Never moved automatically. May lie anywhere in 1–65535, inside or outside the dynamic range. |
| **Block** reservation | A run of consecutive ports allocated automatically from the dynamic range (default 12000–29999, 5 ports). |
| **Assignment** | A named *intent*: "host port H (protocol, bind address) should be published to container port C". Must fall inside one of the stack's own reservations. |
| **Observed mapping** | What `docker compose config` says the stack actually publishes. Recorded in the manifest only with `--record`, and kept separate from assignments. |

## Allocation fields

Every allocation stores:

| Field | Values | Default |
|-------|--------|---------|
| `type` | `fixed` or `block` | – |
| `start`, `end` | 1–65535, `start <= end` | `end = start` for single ports |
| `protocol` | `tcp`, `udp`, or `both` | `tcp` for fixed ports; blocks are always `both` |
| `bind_address` | canonical IP; `0.0.0.0` or `::` mean "all addresses" | `0.0.0.0` |
| `purpose` | free text | blocks: `expansion` |

Assignments store `name`, `host_port`, `container_port`, `protocol` (`tcp`
or `udp`, default `tcp`), `bind_address` (default `0.0.0.0`), `purpose` and
`status` (`assigned`). An assignment status is never `active`/`published`:
whether a port is published is only ever determined by an audit.

Blocks cover TCP *and* UDP on all addresses, so a service in a block can use
either protocol without another reservation.

## Dynamic allocation (first-fit)

1. Collect every existing reservation that could overlap a new wildcard
   TCP+UDP block — i.e. **all** reservations, regardless of protocol or bind
   address — plus any host sockets that are currently listening.
2. Scan the dynamic range from the lowest port upwards and take the first gap
   of `block_size` consecutive free ports.
3. If no gap exists the command fails with "dynamic range exhausted"
   (exit 3). Nothing is written.

Unused ports in a block stay reserved for their stack; they are never handed
to another stack because nothing is listening on them. Freed blocks (via
`release --block` or `delete --release-ports`) are reused by later first-fit
allocations.

Block size defaults to `block_size` in the config and can be overridden per
allocation with `--block-size`. `--no-block` registers a stack with only its
fixed ports.

## Conflict rules

Two reservations **conflict** when all three hold:

1. **Port ranges overlap.**
2. **Protocols overlap**: same protocol, or either side is `both`.
   TCP 53 and UDP 53 do **not** conflict.
3. **Bind scopes overlap**: the addresses are equal, or either side is a
   wildcard (`0.0.0.0` or `::`).
   - Wildcards are treated conservatively across address families:
     `0.0.0.0` conflicts with `::1`, and `::` conflicts with `192.168.1.10`.
     (On Linux, `::` is often dual-stack, and Docker may publish both.)
   - Two different specific addresses (`192.168.1.10` vs `192.168.1.11`)
     do not conflict.

Conflicts are checked against the *complete proposed registry* before any
file is written, including conflicts within the same stack (e.g. reserving a
fixed port inside the stack's own block is rejected as redundant overlap).

The registry is the only authority on ownership. A port that is absent from
the registry is **not** assumed to be free: block allocation also skips host
ports that are currently listening, and audits report listeners that no
reservation explains.

## Assignment rules

* An assignment's host port must be **covered** by one of the stack's own
  reservations: the range contains the port, the protocol matches (or is
  `both`), and the reservation's bind address is a wildcard or equal to the
  assignment's bind address. A wildcard assignment is *not* covered by a
  reservation on a specific address.
* Without `--host-port`, the lowest numbered covered port that is not already
  assigned (same protocol, overlapping bind scope) is chosen. Fixed ports are
  therefore preferred when they are lower than the block.
* Ports outside the stack's reservations are rejected with a hint to run
  `ports reserve` first.
* Releasing an assignment never releases the reservation. Releasing a
  reservation that an assignment still uses is refused.

## Audit classification

`ports check <stack>` and `ports audit` run `docker compose config --format
json` in the stack directory (with the registered or configured Compose
files), then compare:

| Severity | Code | Meaning |
|----------|------|---------|
| violation | `outside-allocation` | Published port is not covered by the stack's reservations. |
| violation | `reserved-by-other-stack` | …and another stack reserves it. |
| violation | `unassigned-mapping` | Published inside the reservations but not registered as an assignment. |
| violation | `assignment-mismatch` | Published host port matches an assignment but container port/protocol differs. |
| violation | `duplicate-publish` | The same host port/protocol/scope is published twice. |
| violation | `manifest-mismatch` / `manifest-problem` | Local `.ports` disagrees with the registry or cannot be read. |
| violation | `registry-conflict` | The registry itself contains overlapping reservations. |
| error | `compose-invalid` | Compose failed (malformed YAML, missing file, …). |
| error | `unresolved-variable` | Compose warned that a variable is unset. |
| error | `docker-unavailable` | `docker compose` could not be run. |
| warning | `assignment-not-published` | Registered assignment not present in Compose. |
| warning | `ephemeral-port` | Compose publishes a container port on a random host port. |
| warning | `host-network` | Service uses `network_mode: host`; ports cannot be audited. |
| warning | `host-listener` | A host socket is listening on a port this stack reserves but does not publish (ownership unknown). |
| warning | `unreserved-listener` (audit only) | A host socket listens inside the dynamic range on a port nobody reserves. |
| info | `host-listener-published` | Port is listening and this stack publishes it — probably the stack itself. |
| info | `host-inspection-unavailable` | Socket tables could not be read; host checks are incomplete. |

Exit status: `3` if any violation, else `4` if any error (configuration
problems are kept distinct from allocation conflicts), else `0`.

Host sockets are read from `/proc/net/{tcp,tcp6,udp,udp6}` (listening TCP
and bound UDP sockets). No privileges are needed, but the owning process is
not resolved, so listeners are reported as "ownership unknown". A clean audit
never guarantees a port will be free when Docker binds it.

## Files and write ordering

The registry (`/var/lib/ports/registry.json`) is authoritative; each stack's
`.ports` manifest is a convenience copy that lives next to the stack.

All mutating commands follow the same protocol:

1. Take an exclusive `flock` on `registry.json.lock` (readers take a shared
   lock). Concurrent `ports new` invocations are therefore serialised and can
   never allocate the same block. The lock is advisory: tools that edit the
   files directly bypass it.
2. Load and fully validate the registry. Corrupt JSON or an unknown schema
   version aborts the command; the file is never rewritten.
3. For an existing stack, verify the manifest matches the registry. If it
   does not, the command fails with exit 6 and asks for `ports sync`.
4. Apply the change to an in-memory copy and validate the whole proposed
   registry (conflicts, ranges, names).
5. Write the manifest (temp file in the same directory → `fsync` → rename →
   `fsync` directory). New manifests are created with a no-replace link so an
   existing file is never clobbered.
6. Write the registry the same way. **This rename is the commit point.**
7. If step 6 fails, the previous manifest content is restored (or the new
   manifest removed). `delete` first archives the manifest as
   `.ports.deleted-<time>` and renames it back if the registry write fails.

Each individual file write is atomic; the pair is **not** a transaction. If
the process dies between steps 5 and 6, the manifest is ahead of the registry.
Step 3 detects this on the next mutation, and `ports sync --apply` rewrites
the manifest from the registry (keeping the old one as `.ports.bak-<time>`).

### Reconciliation (`ports sync`)

`sync` (dry run by default) compares every registered stack with its manifest
and scans the stack root for unregistered manifests.

| Situation | `--apply` action |
|-----------|------------------|
| Manifest missing or corrupt, or differs from registry | Rewrite from registry (old file kept as `.ports.bak-<time>`). |
| Unregistered valid manifest that does not conflict | Import into the registry. |
| Unregistered manifest that conflicts, or names another stack | Report only. |
| Unsupported manifest version, symlinked manifest/dir | Report only; never modified. |
| Registered stack whose directory is missing | Report only (`ports delete` if intended). |

### Recovering a lost or corrupt registry

`ports init --recover` rebuilds the registry from all valid manifests under
the stack root. A corrupt registry is copied aside as
`registry.json.corrupt-<time>` first. Recovery aborts without writing if any
manifest is unreadable or the manifests conflict with each other. An
unsupported registry version is never touched.

## Path safety

* Stack names must match `^[a-z0-9][a-z0-9_-]{0,62}$` and must not be a
  command name, so they can never contain `/` or `..`.
* The stack directory must be a real directory directly under the stack
  root (after resolving the root's own symlinks). A symlinked stack directory
  or `.ports` file is refused.
* Manifests are opened with `O_NOFOLLOW`, and temp files are created in the
  same directory and renamed into place.
* Residual risk: a local user who can write to the stack root could swap a
  stack directory between the check and the rename. Keep the stack root
  writable only by trusted users.
