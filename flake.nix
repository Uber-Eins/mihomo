{
  description = "Uber-Eins Mihomo smart fork, built from source";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      revision = self.shortRev or self.dirtyShortRev or "dev";
      # Source timestamps, not the wall clock, keep build metadata reproducible.
      buildTime = self.lastModifiedDate or "19700101000000";
      version = "alpha-smart-${revision}";
      mkPackage =
        pkgs:
        pkgs.callPackage ./nix/package.nix {
          inherit version buildTime;
        };
    in
    {
      packages = forAllSystems (
        system:
        let
          package = mkPackage nixpkgs.legacyPackages.${system};
        in
        {
          default = package;
          mihomo-smart = package;
          # Compatibility with consumers of the historical flake.
          mihomo-meta = package;
        }
      );

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = nixpkgs.lib.getExe self.packages.${system}.default;
        };
      });

      overlays.default = final: _prev: {
        mihomo-smart = mkPackage final;
        mihomo-meta = final.mihomo-smart;
      };
      # Keep the old singular output usable while offering the standard one.
      overlay = self.overlays.default;

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          package = self.packages.${system}.default;
        in
        {
          inherit package;
          smart-config = pkgs.runCommand "mihomo-smart-config-check" { } ''
            export HOME="$TMPDIR/home"
            mkdir -p "$HOME" "$TMPDIR/state"
            ${pkgs.lib.getExe package} -v > version.txt
            grep -F 'alpha-smart-' version.txt
            grep -F 'with_gvisor' version.txt
            test -x ${package}/bin/mihomo-meta
            ${pkgs.lib.getExe package} -t -d "$TMPDIR/state" \
              -f ${./nix/smart-smoke.yaml}
            touch "$out"
          '';
        }
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
            ];
            CGO_ENABLED = "0";
            GOTOOLCHAIN = "local";
          };
        }
      );
    };
}
