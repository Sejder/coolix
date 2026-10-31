# coolix

A NixOS-native self-hosting PaaS in the spirit of Coolify and Dokploy, minus
the Docker: apps are Nix flakes, run as hardened systemd units, and everything
deployed is recorded in a git repo.

> Status: early development (milestone 1 of 6). Not ready for use.

## How it works

- `/var/lib/coolix/repo` is a git repo holding `apps/<name>/app.toml`; its
  `flake.nix` is generated from the app list and pins every app as a flake input.
- `coolix deploy` locks inputs, builds an **app profile**
  (`/nix/var/nix/profiles/coolix/apps`) and activates it: units are linked into
  `/run/systemd/system` and changed apps restart. Your host's
  `nixos-rebuild` is never involved.
- Each app needs a flake exposing `packages.<system>.default` that reads `$PORT`.

## Usage

```nix
# flake.nix of your NixOS host
inputs.coolix.url = "…";
# in your configuration
imports = [ inputs.coolix.nixosModules.default ];
services.coolix.enable = true;
```

```sh
sudo coolix app add blog --source github:me/blog --domain blog.example.com
sudo coolix deploy blog
sudo coolix app list
```

## Development

```sh
nix develop -c go test ./...
nix build .#checks.x86_64-linux.vm-basic -L   # NixOS VM test, needs KVM
```
