# NixOS options

All options live under `services.coolix`.

| Option | Default | Description |
|---|---|---|
| `enable` | `false` | Enable coolix. |
| `package` | this flake's | The coolix package. |
| `stateDir` | `/var/lib/coolix` | Apps repo and operational state (owned by the `coolix` user). |
| `portRange.from` / `.to` | `20000` / `29999` | Ports coolix may allocate; each app takes two. |
| `drainTime` | `"2s"` | How long the old slot keeps running after traffic moved. |
| `keepGenerations` | `20` | Profile generations kept for rollback (0 = all). |
| `caddy.enable` | `true` | Route app domains through Caddy (enables `services.caddy`). |
| `caddy.tls` | `"acme"` | `acme` (automatic HTTPS), `internal` (Caddy's local CA) or `off` (plain HTTP). |
| `caddy.acmeEmail` | `null` | Contact email for ACME. |
| `caddy.openFirewall` | `false` | Open ports 80 and 443. |
| `ui.enable` | `true` | Run the web UI. |
| `ui.listen` | `"127.0.0.1:8090"` | Where the UI listens. |
| `ui.domain` | `null` | Serve the UI through Caddy on this domain. |
| `ui.adminPasswordFile` | `null` | File with the admin password (required with `ui.enable`); read via `LoadCredential`, so it can be root-only. |

## Paths

| Path | What |
|---|---|
| `/var/lib/coolix/repo` | The apps repo (git). |
| `/nix/var/nix/profiles/coolix/apps` | App profile generations. |
| `/run/systemd/system/coolix-*` | Units of the current profile (re-linked at boot). |
| `/run/coolix/caddy/*.caddy` | Active routes, imported by the host Caddyfile. |
| `/var/lib/coolix-apps/<name>` | App working/state directory. |
| `/var/lib/coolix-db/<app>-<kind>` | Database data. |
| `/var/lib/coolix-secrets/<app>.cred` | Encrypted secrets (root only). |
