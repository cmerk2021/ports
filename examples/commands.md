# Example workflows

All commands accept `--json`. Exit codes: 0 ok, 1 error, 2 usage, 3 conflict /
audit violations, 4 audit incomplete, 5 not found, 6 registry/manifest
inconsistent.

## New server

```console
$ sudo install -m 0755 bin/ports /usr/local/bin/ports
$ sudo install -d /etc/ports && sudo install -m 0644 examples/config.json /etc/ports/config.json
$ sudo ports init
Created registry /var/lib/ports/registry.json
```

## Normal workflow

```console
$ ports new jellyfin --fixed 8096 --purpose web
Registered stack jellyfin (/opt/stacks/jellyfin)
  reserved fixed 8096/tcp on 0.0.0.0 (web)
  reserved block 12000-12004/both on 0.0.0.0 (expansion)

$ ports new nginx-proxy-manager --fixed 80,81,443
Registered stack nginx-proxy-manager (/opt/stacks/nginx-proxy-manager)
  reserved fixed 80/tcp on 0.0.0.0
  reserved fixed 81/tcp on 0.0.0.0
  reserved fixed 443/tcp on 0.0.0.0
  reserved block 12005-12009/both on 0.0.0.0 (expansion)

$ ports new pihole --fixed 53/udp,53/tcp --no-block     # TCP and UDP 53 are separate
$ ports new sonarr --block-size 10                       # 12010-12019
$ ports new newapp --init-dir                            # also creates /opt/stacks/newapp

$ ports assign jellyfin web 8096 --purpose "web UI"
Assigned web: 0.0.0.0:8096 -> 8096/tcp (web UI) in stack jellyfin
Compose port entry: "8096:8096"

$ ports assign jellyfin dlna 1900 --protocol udp --purpose "DLNA discovery"
Assigned dlna: 0.0.0.0:12000 -> 1900/udp (DLNA discovery) in stack jellyfin
Compose port entry: "12000:1900/udp"

$ ports assign jellyfin https 8920 --host-port 12003
$ ports reserve jellyfin 7359/udp --purpose discovery
$ ports add-block jellyfin                               # another expansion block

$ ports jellyfin            # or: ports show jellyfin [--no-compose]
$ ports list [--json]
$ ports find 8096 [--protocol tcp]
$ ports check jellyfin      # audit one stack
$ ports audit               # audit everything, incl. host listeners
$ ports audit --record      # also store observed mappings in each .ports
```

## Error cases

```console
$ ports new other --fixed 443
ports: fixed 443/tcp on 0.0.0.0 conflicts with existing reservation(s): nginx-proxy-manager [fixed 443/tcp on 0.0.0.0]; fixed ports are never moved automatically
$ echo $?
3

$ ports assign jellyfin api 8080 --host-port 9000
ports: host port 0.0.0.0:9000/tcp is not reserved by stack "jellyfin"; reserve it first with `ports reserve jellyfin 9000 --protocol tcp` (or choose a port from `ports jellyfin`)

$ ports new jellyfin
ports: stack "jellyfin" is already registered (see `ports jellyfin`)   # exit 3

$ ports new ../etc --init-dir
ports: invalid stack name "../etc": use 1-63 lowercase letters, digits, '-' or '_', starting with a letter or digit   # exit 2

$ ports 8096
ports: stack "8096" is not registered (to look up a port use `ports find 8096`)   # exit 5

$ ports check jellyfin      # compose.yml publishes "8080:8096"
  VIOLATION outside-allocation ...                          # exit 3

$ ports check broken        # malformed compose.yml
  ERROR     compose-invalid   yaml: line 3: ...             # exit 4
```

## Releasing and deleting

```console
$ ports release jellyfin https                 # assignment only; port stays reserved
$ ports release jellyfin --fixed 7359          # asks for confirmation (or --yes)
$ ports release jellyfin --block               # refused while assignments use it
ports: assignment "dlna" (12000/udp) uses the reservation; release the assignment first with `ports release jellyfin dlna`

$ ports delete pihole
ports: deleting stack "pihole" would free its reservations (fixed 53/tcp on 0.0.0.0; fixed 53/udp on 0.0.0.0) for other stacks; re-run with --release-ports to confirm (application data in /opt/stacks/pihole is never deleted)
$ ports delete pihole --release-ports          # interactive "yes", or add --yes
```

## Inconsistent or corrupt state

```console
$ ports assign jellyfin x 1                    # someone edited .ports by hand
ports: cannot use manifest for stack "jellyfin": ...; run `ports sync --dry-run` to review and `ports sync --apply` to repair
$ echo $?
6

$ ports sync                                   # same as --dry-run
jellyfin: manifest-corrupt
    file is corrupt: /opt/stacks/jellyfin/.ports: missing "version" field
  proposed: preserve the corrupt file as .ports.bak-<time> and rewrite it from the registry [safe]

Dry run: nothing was changed. Re-run with --apply to perform the safe repairs.

$ sudo ports sync --apply

$ ports list                                   # registry damaged
ports: file is corrupt: /var/lib/ports/registry.json: invalid JSON: unexpected end of JSON input (the registry was not modified; repair it by hand or run `ports init --recover`)
$ sudo ports init --recover                    # rebuild from .ports manifests; old file kept
```
