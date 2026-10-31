# How coolix works

## The pieces

```
              web UI (coolix serve)        CLI (sudo coolix …)
                         \                  /
                          coolix user: edits the apps repo, runs nix builds
                                    |
                 systemctl start coolix-activate@<profile>.service  (polkit)
                                    |
                     root: switch profile, apply secrets, activate
                                    |
     /run/systemd/system/coolix-app-<name>@.service   coolix-db-<name>-<kind>.service
                                    |
                    Caddy (/run/coolix/caddy/<name>.caddy) → 127.0.0.1:<slot port>
```

## The apps repo

`/var/lib/coolix/repo` is a git repository and the source of truth:

```
flake.nix               generated: pins nixpkgs, the coolix lib and one input per app
flake.lock              pins every app to an exact revision
apps/<name>/app.toml    the app's definition (see app-toml.md)
apps/<name>/override.nix  optional, hand-written: extra unit settings
```

Everything coolix changes (adding an app, setting an env var, a deploy's lock
update, a rollback) is a commit. The repo describes the *desired* state; if a
deploy fails, `coolix status` and the dashboard show that the running state
differs.

## Deploying

1. `nix flake lock` (and `nix flake update app-<name>` when deploying a single
   app) pins the sources; the result is committed.
2. `nix build .#coolixProfile` builds an **app profile**: every app's unit
   file, its database units and an `apps.json` manifest. The build runs as the
   unprivileged `coolix` user through the nix daemon.
3. The `coolix` user asks systemd to start
   `coolix-activate@<store-path>.service`. Polkit allows this unit pattern and
   nothing else. As root, it:
   - validates the store path and makes it a new generation of
     `/nix/var/nix/profiles/coolix/apps`,
   - encrypts pending secret changes into `/var/lib/coolix-secrets`,
   - links the units into `/run/systemd/system` (outside NixOS's control, so
     `nixos-rebuild` never touches apps; a boot service re-links them),
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
file is linked back in, and the whole previous generation is re-activated. The
running state then matches a generation again. Because systemd keeps running
processes on the unit definition they started with, swapping the template file
under a running slot is safe.

## Rollback

`coolix rollback [GEN]` re-activates an older generation's store path (still in
the store, since generations are GC roots) with the same blue/green switch,
then commits "Rollback to generation N", restoring the repo tree to the
revision that generation was built from. Older generations beyond
`keepGenerations` are deleted after each successful activation and become
garbage-collectable.

## Databases

A database is a plain unit in the profile (`coolix-db-<app>-postgres.service`).
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
it into a private, non-swappable directory, and a tiny wrapper exports it as
environment variables before exec'ing the app. A secret change restarts the
app through the normal blue/green switch.

## Security model

- Apps are hardened systemd units (`DynamicUser`, `ProtectSystem=strict`,
  `PrivateTmp`, `PrivateDevices`, `NoNewPrivileges`, a `@system-service`
  syscall filter, no capabilities, …). `[hardening]` in `app.toml` overrides
  individual keys.
- The web UI runs as `coolix`, not root. It has a single admin password (read
  through `LoadCredential`), HMAC-signed `SameSite=Strict` session cookies,
  Origin checks on every POST, and a strict Content-Security-Policy.
- **The `coolix` user is root-equivalent in practice:** it decides which units
  run, and a unit can opt out of `DynamicUser`. The split keeps root away from
  git and from evaluating app code, and keeps a compromised UI from trivially
  running commands as root, but treat the admin password like a root password.
