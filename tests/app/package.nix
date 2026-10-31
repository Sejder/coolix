# Serves `version` at / and, unless the version is "broken", 200 at /healthz.
{ pkgs, version }:

let
  www = pkgs.runCommandLocal "www" { } ''
    mkdir $out
    echo ${version} > $out/index.html
    ${pkgs.lib.optionalString (version != "broken") "echo ok > $out/healthz"}
  '';
in
pkgs.writeShellScriptBin "hello-server" ''
  exec ${pkgs.python3}/bin/python3 -m http.server "$PORT" --bind 127.0.0.1 --directory ${www}
''
