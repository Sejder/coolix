# NixOS options

All options live under `services.coolix`.

| Option | Default | Description |
|---|---|---|
| `enable` | `false` | Enable coolix. |
| `package` | this flake's | The coolix package. |
| `stateDir` | `/var/lib/coolix` | Apps repo and operational state (owned by the `coolix` user). |
| `dataDir` | `null` | Where app data, databases and backups live (`<dataDir>/{apps,db,backups}`), e.g. on a bigger disk. Bind-mounted where systemd keeps dynamic users' state; `null` keeps them in `/var/lib`. |
| `secretsDir` | `/var/lib/coolix-secrets` | Encrypted app secrets (root only). |
| `portRange.from` / `.to` | `20000` / `29999` | Ports coolix may allocate; each app takes two. Must end below 32768 (the kernel's ephemeral ports). |
| `drainTime` | `"2s"` | How long the old slot keeps running after traffic moved. |
| `keepGenerations` | `20` | Profile generations kept for rollback (0 = all). |
| `apps.allowedHardeningOverrides` | see below | `[hardening]` keys apps may set. The default leaves out `PrivateDevices`, `ProtectKernel*`, `ProtectControlGroups` and `RestrictNamespaces`, which widen the kernel attack surface an app can reach. |
| `apps.defaultTasksMax` | `1024` | `TasksMax` for apps without `resources.tasks_max` (0: systemd's default). |
| `apps.defaultMemoryMax` | `null` | `MemoryMax` for apps without `resources.memory_max`. |
| `caddy.enable` | `true` | Route app domains through Caddy (enables `services.caddy`). |
| `caddy.tls` | `"acme"` | `acme` (automatic HTTPS), `internal` (Caddy's local CA) or `off` (plain HTTP). |
| `caddy.acmeEmail` | `null` | Contact email for ACME. |
| `caddy.openFirewall` | `false` | Open ports 80 and 443. |
| `previews.domain` | `null` | Previews of branches and pull requests are served at `<app>-<branch or prN>.<domain>` (needs a wildcard DNS record). Apps can set their own `preview.domain`. |
| `git.enable` | `false` | Deploy push apps (`source = "push"`) with `git push coolix@host:APP`. Needs `services.openssh`. Logins of the `coolix` user can only push and fetch: sshd forces `coolix ssh-serve`, with no terminal and no forwarding. |
| `git.host` / `git.port` | `ui.domain` or the host name / the first `services.openssh.ports` | Shown in push remotes. |
| `git.authorizedKeys` | `[]` | SSH public keys that may push to every push app. More can be added in the UI or with `coolix ssh-key add`. |
| `ui.enable` | `true` | Run the web UI. |
| `ui.listen` | `null` | TCP address for the UI. By default it listens on `/run/coolix/ui.sock`, which only Caddy and root can open (apps share the host network); without Caddy, `127.0.0.1:8090`. Over SSH: `ssh -L 8090:/run/coolix/ui.sock host`, then `http://localhost:8090` (passkeys work there too). |
| `ui.domain` | `null` | Serve the UI through Caddy on this domain. Passkeys work there (over HTTPS) and on `http://localhost:<port>`. |
| `ui.externalUrl` | `null` | The UI's public URL when your own reverse proxy (nginx, …) serves it instead of `ui.domain`, e.g. `https://coolix.example.com`. Passkeys work there, cookies are `Secure` for HTTPS, and apps can't claim the domain. |
| `ui.socketGroup` | Caddy's group | Group that may open `/run/coolix/ui.sock`. For nginx: `"nginx"`, with `proxyPass = "http://unix:/run/coolix/ui.sock"`. Prefer this to `ui.listen`: apps share the host network and could reach a TCP port directly. |
| `ui.adminPasswordFile` | `null` | Optional declarative admin password (12+ characters, e.g. from agenix or sops-nix), read via `LoadCredential`. Without it, the first visit shows a setup page that needs a code from `sudo coolix setup-code`. |

Default `apps.allowedHardeningOverrides`: `MemoryDenyWriteExecute`,
`PrivateTmp`, `ProtectHome`, `ProtectHostname`, `ProtectClock`,
`RestrictRealtime`, `LockPersonality`, `UMask`. `RestrictAddressFamilies` and
`SystemCallFilter` are opt-in, and even then only vetted additions are
accepted (`AF_NETLINK`; groups like `@pkey`, `@timer`, `@keyring`), never
`@known`, `@privileged`, `@debug`, `bpf` and the like.

The module also moves Caddy's admin API to a unix socket
(`/run/caddy/admin.sock`), because apps share the host network and could
otherwise reconfigure Caddy through `localhost:2019`. Other host Caddy sites
(`services.caddy.virtualHosts`) are reserved: apps can't claim those domains.

## The admin account

- **Setup code:** on a fresh install, run `sudo coolix setup-code` and enter
  the code on the setup page within an hour. Then add a passkey and/or a
  password on the Account page.
- **Lost access:** `sudo coolix account reset` removes the password and
  passkeys; claim the account again with a new setup code.
- **Declarative:** `ui.adminPasswordFile` always works in addition to
  whatever is set up in the UI.

## Paths

| Path | What |
|---|---|
| `<stateDir>/repo` | The apps repo (git). |
| `<stateDir>/account.json` | Admin account: password hash and passkeys. |
| `<stateDir>/webhooks/<app>.token` | Webhook tokens. |
| `<stateDir>/secrets-pending/` | Secret changes not deployed yet (0600). |
| `/nix/var/nix/profiles/coolix/apps` | App profile generations. |
| `/run/coolix/units/` | Units rendered by root for the current profile. |
| `/run/systemd/system/coolix-*` | Links to them (re-rendered at boot). |
| `/run/coolix/control.sock` | Privileged requests (activate, backup, restore); group `coolix`. |
| `/run/coolix/query.sock` | App logs and backup listings; group `coolix`. |
| `/run/coolix/caddy/*.caddy` | Active routes, imported by the host Caddyfile. |
| `/etc/coolix/coolix` | The coolix binary rendered units run (stable across upgrades). |
| `/var/lib/coolix-apps/<name>` | App working/state directory (`<dataDir>/apps/<name>` with `dataDir`). |
| `/var/lib/coolix-db/<app>-<kind>` | Database data (`<dataDir>/db/…`). |
| `/var/lib/private/coolix-backups/<app>` | Database dumps, the app's user only (`<dataDir>/backups/…`). |
| `<secretsDir>/<app>.cred` | Encrypted secrets (root only). |

## Moving data to `dataDir`

Setting `dataDir` on an existing install mounts empty directories over the
old ones, so databases would start fresh. Move the data first:

1. Stop the apps: `systemctl stop 'coolix-app-*' 'coolix-db-*'`.
2. Copy `/var/lib/private/coolix-{apps,db,backups}` to
   `<dataDir>/{apps,db,backups}`, keeping owners and modes (`cp -a`).
3. Set `dataDir`, `nixos-rebuild switch`, then `sudo coolix deploy`.

## Upgrades and `nixos-rebuild switch`

Switching to a new coolix or changed options doesn't touch running apps:
units are re-rendered by the next `coolix deploy`, or at boot.

## Upgrading from a version before root-rendered units

Profiles built before coolix rendered units itself can't be activated
anymore. After upgrading, apps don't start at boot (`coolix status` says so)
until you run `sudo coolix deploy` once. App names longer than 20 characters
and `override.nix` files are no longer accepted.
