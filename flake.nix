{
  description = "Development environment for shenmux";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { nixpkgs, ... }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];

      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system:
          f (import nixpkgs { inherit system; }));
    in {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            git
            gnumake
            nodejs
            pkg-config
            zeromq
          ];

          shellHook = ''
            export CGO_ENABLED=1
            # Keep Unix-domain socket paths short on macOS, whose path limit
            # is easy to hit with Nix's generated temporary directory names.
            export TMPDIR=/tmp
          '';
        };
      });
    };
}
