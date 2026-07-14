{ pkgs ? import
    (fetchTarball {
      name = "jpetrucciani-2026-07-13";
      url = "https://github.com/jpetrucciani/nix/archive/2d1539d70dd16a5fe473f6e4ce08713dd11cadcc.tar.gz";
      sha256 = "1jmah2mypq5md9cblgasaa945ds3fqzirv4hp5dxkicb9hz3064m";
    })
    { }

}:
let
  name = "ge-mcp";

  tools = with pkgs; {
    cli = [
      jfmt
      nixup
    ];
    go = [
      go
      go-tools
      gopls
      gcc
    ];
    scripts = pkgs.lib.attrsets.attrValues scripts;
  };

  scripts = with pkgs; { };
  paths = pkgs.lib.flatten [ (builtins.attrValues tools) ];
  env = pkgs.buildEnv {
    inherit name paths; buildInputs = paths;
  };
in
(env.overrideAttrs (_: {
  inherit name;
  NIXUP = "0.0.11";
})) // { inherit scripts; }
