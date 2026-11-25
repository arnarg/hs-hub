{
  description = "Publishes services announced by tailnet nodes as Headscale extra DNS records";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        hs-hub = pkgs.callPackage ./nix/package.nix { };
        default = hs-hub;
      });

      nixosModules = rec {
        hs-hub =
          { pkgs, ... }:
          {
            imports = [ ./nix/module.nix ];
            services.hs-hub.package =
              nixpkgs.lib.mkDefault
                self.packages.${pkgs.stdenv.hostPlatform.system}.hs-hub;
          };
        default = hs-hub;
      };

      checks = forAllSystems (pkgs: {
        inherit (self.packages.${pkgs.stdenv.hostPlatform.system}) hs-hub;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gcc # for `go test -race`
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
