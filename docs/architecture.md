# How coolix works

## The pieces

```
              web UI (coolix serve)        CLI (sudo coolix …)
                         \                  /
                          coolix user: edits the apps repo, runs nix builds
                                    |
            /run/coolix/control.sock → coolix-control@ (root, fixed requests)
                                    |
              root: validate the profile, render units, apply secrets, activate
                                    |
     /run/systemd/system/coolix-app-<name>@.service   coolix-db-<name>-<kind>.service
     (links to /run/coolix/units/…, written by root) coolix-backup-<name>.{service,timer}
                                    |
                    Caddy (/run/coolix/caddy/<name>.caddy) → 127.0.0.1:<slot port>
```

## The apps repo

`/var/lib/coolix/repo` is a git repository and the source of truth:

```
flake.nix               generated: pins nixpkgs, the coolix lib and one input per app
flake.lock              pins every app to an exact revision
apps/<name>/app.toml    the app's definition (see app-toml.md)
```

Everything coolix changes (adding an app, setting an env var, a deploy's lock
update, a rollback) is a commit. The repo describes the *desired* state; if a
deploy fails, `coolix status` and the dashboard show that the running state
differs.

## Deploying

1. `nix flake lock` (and `nix flake update app-<name>` when deploying a single
   app) pins the sources, in a private clone of the repo so edits in the UI
   aren't blocked while inputs are fetched; the result is committed.
2. `nix build` of the committed revision builds an **app profile**: an
   `apps.json` manifest with each app's store paths (its program, database
   servers, runtime packages) and settings. It contains no unit files. The
   build runs as the unprivileged `coolix` user through the nix daemon.
3. The `coolix` user sends `activate <store path>` to
   `/run/coolix/control.sock` (group `coolix`). A root `coolix-control@`
   instance handles the request and streams its output back:
   - validates the store path and **every field of the manifest** against a
     fixed policy (see [Security model](#security-model)), and refuses the
     profile if anything is off,
   - encrypts pending secret changes into `/var/lib/coolix-secrets`,
   - makes it a new generation of `/nix/var/nix/profiles/coolix/apps`,
   - renders the units itself into `/run/coolix/units` and links them into
     `/run/systemd/system` (outside NixOS's control, so `nixos-rebuild` never
     touches apps; a boot service re-renders them),
   - starts or restarts changed databases, then switches changed apps.

## Blue/green

Each app's unit is a template, `coolix-app-<name>@.service`, with two
instances: `blue` on `port_base` and `green` on `port_base + 1`. To switch, the
activator:

1. starts the idle slot with the new unit file,
2. polls the health check until it passes or the timeout expires,
3. rewrites the app's Caddy snippet to the new port and reloads Caddy (a
   graceful reload),
4. waits the drain time, then stops the old slot.

If the health check fails, the new slot is stopped, the app's previous unit
file is linked back in, the previous generation is re-activated and the failed
one deleted. The running state then matches a generation again. Because systemd
keeps running processes on the unit definition they started with, swapping the
template file under a running slot is safe. App slots only `Wants=` their
databases, so restarting a changed database doesn't restart the serving slot,
and a removed database is stopped only after the new slot is up. An app that
wasn't changed but is down (e.g. crash-looping) is reported, but doesn't fail
the deploy of other apps.

At boot, `coolix-activate-boot` re-renders the current generation. Apps that
no longer pass the policy (say, after narrowing `portRange`) are skipped with a
warning; a profile built by an older coolix is left alone until the next
`coolix deploy`.

## Rollback

`coolix rollback [GEN]` re-activates an older generation's store path (still in
the store, since generations are GC roots) with the same blue/green switch,
then commits "Rollback to generation N", restoring the repo tree to the
revision that generation was built from. Older generations beyond
`keepGenerations` are deleted after each successful activation and become
garbage-collectable.

## Databases

A database is a plain unit (`coolix-db-<app>-postgres.service`).
The database and both app slots run as the same `DynamicUser`
(`coolix-app-<name>`). The socket directory is `0700` and owned by that user,
and Postgres uses peer authentication. Only the app can connect, and there are
no passwords. Data lives in `/var/lib/coolix-db/<app>-<kind>` and survives
removing the database from the app.

## Secrets

The UI and CLI only write *pending* changes (`/var/lib/coolix/secrets-pending`,
mode 0600) and commit the secret's **name** to `app.toml`. During activation,
root merges them into `/var/lib/coolix-secrets/<app>.cred`, encrypted with
`systemd-creds` (TPM2 if present, plus the host key), and deletes the pending
file. The unit loads it with `LoadCredentialEncrypted=env:…`. systemd decrypts
it into a private, non-swappable directory, and `coolix exec-app` (the
wrapper every app unit starts through) exports it as environment variables
before exec'ing the app. A secret change restarts the
app through the normal blue/green switch.

## Backups

Every app with PostgreSQL gets `coolix-backup-<app>.service`, which runs
`pg_dump` as the app's user into `/var/lib/private/coolix-backups/<app>` and
keeps the newest `backup.keep` dumps (default 7). With `backup.schedule` set, a
`coolix-backup-<app>.timer` runs it on that schedule (`Persistent=true`, so a
missed run happens after boot). `coolix db backup` and the UI ask coolix-control to start it on demand.
`coolix db restore` has it start `coolix-restore-<app>@<dump>.service`, which runs
`pg_restore --clean --single-transaction` into the live database. Dumps are
only readable by the app's user and root. Copy them off the host yourself
(e.g. with restic or borg): a backup on the same disk doesn't protect against
losing the disk.

## Webhooks

`POST /hooks/<app>` on the UI deploys the app (like `coolix deploy <app>`)
when a forge reports a push. Each app has its own random token, stored in
`<stateDir>/webhooks/<app>.token` (not in git), and the request must be
signed with it: an HMAC-SHA256 signature for GitHub (`X-Hub-Signature-256`),
Gitea and Forgejo, the token itself for GitLab (`X-Gitlab-Token`), or
`Authorization: Bearer <token>` for anything else. Pings are answered, other
events and pushes to other branches than `webhook.branch` are ignored. A
matching push deploys the app's `source` as written: set the branch there too
(e.g. `github:you/app/main`) so the filter and the deployed branch agree. The UI
must be reachable from the forge for this, normally via `ui.domain`.

## Security model

There are three levels of trust:

| Who | Can |
|---|---|
| root (`coolix-control@`, `coolix-activate-boot`) | everything; runs only coolix's own code, and only on validated data |
| the `coolix` user (the web UI, `sudo coolix …`) | edit the apps repo, build, deploy any code *into an app's sandbox*, ask root to activate a profile and to back up or restore an app's database, set secrets it can't read back |
| an app | its own state directory, databases, secrets and two ports |

What keeps the `coolix` user, and so a compromised UI or a stolen admin
login, from becoming root:

- **One narrow door to root.** The coolix user has no polkit rights. It
  reaches root only through `/run/coolix/control.sock` (owned by root, group
  `coolix`), which accepts exactly `activate <profile>`, `backup <app>` and
  `restore <app> <dump>`, each validated. (A polkit rule can't be this
  narrow: systemd asks polkit the same question for "start this unit" and
  "create a transient unit with this name and run anything in it".)
- **Root renders every unit.** The profile is data only. Root opens it
  without following symlinks and checks every field before anything happens:
  names, ports (in the configured range, no clashes), domains (fully
  qualified, unique, not the UI's or any other Caddy site of the host), store
  paths, environment names and values, resource limits, calendar expressions,
  and hardening overrides. Then it generates the unit files itself. A
  hand-built "profile" that asks for anything else is refused before it
  becomes a generation.
- **The sandbox is fixed.** Every app and database unit runs with
  `DynamicUser`, `NoNewPrivileges`, an empty capability set,
  `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`,
  `ProtectProc=invisible`, `RestrictSUIDSGID`, a `@system-service` syscall
  filter and more. `[hardening]` in `app.toml` may only relax settings the host
  allows (`services.coolix.apps.allowedHardeningOverrides`), none of which
  grant privileges; syscall and address-family settings can only add to the
  defaults. `override.nix`, which allowed arbitrary unit settings, is gone.
- **Apps can't hijack each other's traffic.** An app may only listen on its
  own two ports (`SocketBindAllow`), so it can't squat another app's idle
  slot or the UI's port and answer their health checks. Databases and backup
  jobs have no network at all. Caddy's admin API, which could reroute every
  domain, is on a unix socket only Caddy can open, not on `localhost:2019`.
- **No back doors to the host.** The web UI listens on a unix socket only
  Caddy and root can open, so apps can't talk to it directly or forge the
  client address it rate-limits by. Apps can't reach the root nix-daemon
  either (`InaccessiblePaths`), so they can't build outside their limits.
- **No journal access.** The coolix user isn't in `systemd-journal`. App logs
  and backup listings come from `coolix-query`: a socket-activated service
  (`/run/coolix/query.sock`, group `coolix`) that runs as a throwaway dynamic
  user, answers a fixed set of questions about coolix units only, and can't
  write anything.
- **Root never follows the coolix user's files.** Pending secrets and the
  profile manifest are read with `O_NOFOLLOW`, size- and type-checked.
- **The web UI** runs as `coolix`, not root. Sign-in is by passkey or
  password (argon2id), rate limited per client and to about one guess per
  second overall. Sessions are HMAC-signed `SameSite=Strict` cookies that are
  revoked on sign-out, password changes and passkey removal. Every POST is
  Origin-checked and there's a strict Content-Security-Policy. A fresh
  install can only be claimed with a one-time code from
  `sudo coolix setup-code`, so whoever finds the URL first can't take it.

Plain `env` values aren't secret: they're in git and in the rendered units,
which any local user can read. Use secrets for anything sensitive. Secret
changes aren't versioned: rolling back a deploy keeps the current secrets.

The coolix user can still run arbitrary code as *any app* (that's what
deploying is) and so read that app's secrets and data. Treat the admin
account as access to all apps, not to the host.

Apps share the host's network namespace, so they can connect to each
other's ports directly (not only through Caddy). Don't rely on network
isolation between apps; authenticate what they expose.
