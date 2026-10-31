{ pkgs }:

pkgs.writeShellScriptBin "dbapp" ''
  set -eu
  export PATH=${pkgs.coreutils}/bin
  out=$(mktemp -d)
  ${pkgs.postgresql}/bin/psql "$DATABASE_URL" -qtAc \
    "CREATE TABLE IF NOT EXISTS boots (at timestamptz DEFAULT now()); INSERT INTO boots DEFAULT VALUES; SELECT count(*) FROM boots" \
    > "$out/pg"
  ${pkgs.redis}/bin/redis-cli -s "''${REDIS_URL#unix://}" INCR boots > "$out/redis"
  printf %s "''${API_TOKEN-unset}" > "$out/secret"
  id -un > "$out/user"
  exec ${pkgs.python3}/bin/python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$out"
''
