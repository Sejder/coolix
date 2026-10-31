# `app.toml` reference

coolix writes these files for you (UI or `coolix app add/set`), but you can
also edit them directly in `/var/lib/coolix/repo/apps/<name>/app.toml` and
commit as the `coolix` user. They're validated on every load, and root
validates the result again before activating it.

```toml
name = "blog"                       # must match the directory; 2-20 chars of a-z, 0-9, -
source = "github:you/blog"          # any flake reference
package = "default"                 # packages.<system>.<package>; default "default"
command = "bin/blog"                # path inside the package; default: meta.mainProgram
pre_start = "bin/migrate"           # path inside the package, run before every start of a slot
pre_start_timeout = "15m"           # how long pre_start may run (default 15m, max 2h)
domains = ["blog.example.com"]      # routed through Caddy; omit for internal-only apps
port_base = 20010                   # allocated by coolix: blue = 20010, green = 20011
follow_nixpkgs = true               # make the app's nixpkgs input follow the host's (default)
path = ["ffmpeg", "imagemagick"]    # nixpkgs attributes whose bin/ is put on PATH
secrets = ["SESSION_KEY"]           # names only; values via `coolix secret set` or the UI

[health]
path = "/healthz"                   # GET must return < 400; omit for a TCP check
timeout = "30s"                     # how long a new slot may take to become healthy (max 10m)

[env]
LOG_LEVEL = "info"                  # plain values; stored in git

[[databases]]
kind = "postgres"                   # sets DATABASE_URL, PGHOST, PGUSER, PGDATABASE
version = "18"                      # PostgreSQL major version; default: 18. Changing it on
                                    # existing data needs a manual pg_upgrade (or dump+restore)

[[databases]]
kind = "redis"                      # sets REDIS_URL (unix socket)

[resources]                         # systemd resource limits for each slot
memory_max = "512M"                 # MemoryMax: bytes with K/M/G/T, a percentage, or "infinity"
cpu_quota = "50%"                   # CPUQuota: 100% is one CPU
tasks_max = 256                     # TasksMax: processes + threads

[backup]                            # postgres only; on-demand backups work without this
schedule = "daily"                  # systemd calendar expression, e.g. "*-*-* 03:00"
keep = 7                            # newest dumps to keep (default 7)

[webhook]
branch = "main"                     # only pushes to this branch deploy (default: any)

[hardening]                         # relax individual sandbox settings
MemoryDenyWriteExecute = false      # e.g. for JIT runtimes (Node, Java, .NET)
```

`pre_start` runs with the app's environment, secrets and working directory,
before *every* start of a slot (deploys, restarts after a crash, reboots), so
it must be idempotent. Database migrations are the typical use. If it fails,
the slot doesn't start, and a deploy keeps the old version running.

Apps can only listen on their own two ports (`port_base` and `port_base + 1`).
Domains must be fully qualified (`app.example.com`, not `localhost` or an IP)
and not used by the host's other Caddy sites.

Reserved variable names: `PORT`, `PATH`, `COOLIX_*`, `DATABASE_URL`,
`REDIS_URL`, `PGHOST`, `PGUSER`, `PGDATABASE`, `CREDENTIALS_DIRECTORY`.

## `[hardening]`

Apps can relax only settings that don't give them privileges, and only those
the host allows (`services.coolix.apps.allowedHardeningOverrides`; see
[options](options.md)):

| Setting | Values |
|---|---|
| `MemoryDenyWriteExecute`, `PrivateDevices`, `PrivateTmp`, `ProtectHome`, `ProtectClock`, `ProtectHostname`, `ProtectKernelLogs`, `ProtectKernelModules`, `ProtectKernelTunables`, `ProtectControlGroups`, `RestrictNamespaces`, `RestrictRealtime`, `LockPersonality` | `true` / `false` |
| `RestrictAddressFamilies` | families to add to `AF_INET AF_INET6 AF_UNIX`, e.g. `"AF_NETLINK"` |
| `SystemCallFilter` | syscalls or groups to add to `@system-service`, e.g. `"@pkey"` |
| `UMask` | e.g. `"0027"` |

Everything else (`DynamicUser`, `User`, `NoNewPrivileges`, capabilities,
`ProtectSystem`, extra `Exec*` commands, …) is fixed. An `app.toml` that tries
to change it is rejected.

## No `override.nix`

Earlier versions merged `apps/<name>/override.nix` into the unit. Units are
now rendered by root from validated data, so arbitrary unit settings aren't
possible, and a deploy fails with a message if the file exists. Use `path`,
`pre_start`, `[resources]` and `[hardening]` instead.
