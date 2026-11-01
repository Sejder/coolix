# `app.toml` reference

coolix writes these files for you (UI or `coolix app add/set`), but you can
also edit them directly in `/var/lib/coolix/repo/apps/<name>/app.toml` and
commit as the `coolix` user. They're validated on every load, and root
validates the result again before activating it.

```toml
name = "blog"                       # must match the directory; 2-20 chars of a-z, 0-9, -
source = "github:you/blog"          # any flake reference, or "push" (see below)
branch = "main"                     # pushes to this branch deploy, by webhook or git push
                                    # (default: any branch for webhooks; main for push apps)
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

[preview]                           # previews of branches and pull requests (see below)
enabled = true
domain = "preview.example.com"      # default: services.coolix.previews.domain
secrets = false                     # give previews this app's secrets (default: no)
max = 5                             # previews at once

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

## Push apps

With `source = "push"`, the app's code is whatever you `git push` to the
host (with `services.coolix.git.enable`):

```sh
coolix app add blog --source push --domain blog.example.com
git remote add coolix coolix@your-host:blog
git push coolix main          # builds and deploys, with the output in your terminal
```

The repo must contain a flake exposing `packages.<system>.default`, like any
other source. Pushes to other branches are stored but don't deploy. SSH keys
come from `services.coolix.git.authorizedKeys`, the account page in the UI,
or `coolix ssh-key add`. The pushed code lives in
`/var/lib/coolix/git/<name>.git`, and is deleted with the app.

The older `[webhook] branch = "..."` still works, and becomes `branch` the
next time coolix saves the app.

## Previews

With `[preview] enabled = true`, branches and pull requests of an app can be
deployed next to it, each as its own app: `blog-pr12.preview.example.com`
for pull request 12, `blog-feature-x.preview.example.com` for a branch.

- **Pull requests** deploy through the app's webhook: opening one or pushing
  to it deploys its preview, and closing or merging it removes the preview.
  Add the pull/merge request event to the webhook (GitHub and Forgejo/Gitea:
  "Pull requests"; GitLab: "Merge request events"). Pull requests from forks
  are never deployed: their code comes from people without push access.
- **Push apps:** `git push coolix feature-x` deploys a preview of
  `feature-x`, and `git push coolix :feature-x` removes it.
- **By hand:** "Deploy preview" on the app's page, or
  `coolix preview deploy blog feature-x`.

A preview gets the app's settings as of when it's deployed, its own ports
and its own empty databases (`pre_start` runs as usual, so migrations
apply). It doesn't get the app's secrets unless `secrets = true`: its code
comes from a branch, not from what you've reviewed. Removing a preview
deletes its data. Previews are listed in `apps/<app>-<…>/app.toml` like other
apps, with `preview_of` and `preview_ref`; coolix rewrites those files, so
don't edit them.
