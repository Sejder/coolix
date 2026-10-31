{ pkgs, version }:

let
  www = pkgs.writeTextDir "index.html" version;
in
pkgs.writeShellScriptBin "hello-server" ''
  exec ${pkgs.python3}/bin/python3 -m http.server "$PORT" --bind 127.0.0.1 --directory ${www}
''
