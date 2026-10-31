{ pkgs }:

let
  # On start it bumps a counter in postgres and redis and publishes the
  # results (plus its secret, environment and pre-start marker) as files.
  app = pkgs.writeShellScriptBin "dbapp" ''
    set -eu
    export PATH=${pkgs.coreutils}/bin:$PATH
    out=$(mktemp -d)
    ${pkgs.postgresql}/bin/psql "$DATABASE_URL" -qtAc \
      "CREATE TABLE IF NOT EXISTS boots (at timestamptz DEFAULT now()); INSERT INTO boots DEFAULT VALUES; SELECT count(*) FROM boots" \
      > "$out/pg"
    ${pkgs.redis}/bin/redis-cli -s "''${REDIS_URL#unix://}" INCR boots > "$out/redis"
    printf %s "''${API_TOKEN-unset}" > "$out/secret"
    printf %s "''${WEIRD-unset}" > "$out/weird"
    id -un > "$out/user"
    cat migrated > "$out/migrated" 2>/dev/null || echo no > "$out/migrated"
    (hello 2>/dev/null || echo none) > "$out/hello"
    # Can it reach the root nix-daemon?
    test -e /nix/var/nix/daemon-socket/socket && echo reachable > "$out/nixd" || echo hidden > "$out/nixd"
    # May it listen on a port that isn't its own (the UI's)?
    ${pkgs.python3}/bin/python3 -c 'import socket; socket.socket().bind(("127.0.0.1", 8090))' 2>/dev/null \
      && echo bound > "$out/squat" || echo denied > "$out/squat"
    exec ${pkgs.python3}/bin/python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$out"
  '';
  # pre_start: runs in the app's state directory before each start.
  migrate = pkgs.writeShellScriptBin "dbapp-migrate" ''
    echo yes > migrated
  '';
in
pkgs.symlinkJoin {
  name = "dbapp";
  paths = [ app migrate ];
  meta.mainProgram = "dbapp";
}
