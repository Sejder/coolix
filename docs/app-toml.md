# `app.toml` reference

coolix writes these files for you (UI or `coolix app add/set`), but you can
also edit them directly in `/var/lib/coolix/repo/apps/<name>/app.toml` and
commit as the `coolix` user. They're validated on every load.

```toml
name = "blog"                       # must match the directory; a-z, 0-9, -
source = "github:you/blog"          # any flake reference
package = "default"                 # packages.<system>.<package>; default "default"
command = "bin/blog"                # path inside the package; default: meta.mainProgram
domains = ["blog.example.com"]      # routed through Caddy; omit for internal-only apps
port_base = 20010                   # allocated by coolix: blue = 20010, green = 20011
follow_nixpkgs = true               # make the app's nixpkgs input follow the host's (default)
secrets = ["SESSION_KEY"]           # names only; values via `coolix secret set` or the UI

[health]
path = "/healthz"                   # GET must return < 400; omit for a TCP check
timeout = "30s"                     # how long a new slot may take to become healthy

[env]
LOG_LEVEL = "info"                  # plain values; stored in git

[[databases]]
kind = "postgres"                   # sets DATABASE_URL, PGHOST, PGUSER, PGDATABASE
version = "18"                      # PostgreSQL major version; default: 18

[[databases]]
kind = "redis"                      # sets REDIS_URL (unix socket)

[hardening]                         # override individual systemd hardening settings
MemoryDenyWriteExecute = false      # e.g. for JIT runtimes (Node, Java, .NET)
```

Reserved variable names: `PORT`, `COOLIX_*`, `DATABASE_URL`, `REDIS_URL`,
`PGHOST`, `PGUSER`, `PGDATABASE`.

## `override.nix`

For anything `app.toml` can't express, add `apps/<name>/override.nix`. It's
merged into the generated unit last:

```nix
{ pkgs, lib, pkg, name, app }:
{
  Service = {
    MemoryMax = "512M";
    ExecStartPre = "${pkg}/bin/migrate";
  };
}
```
