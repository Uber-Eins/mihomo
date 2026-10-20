# Building this fork with Nix

The flake builds **Uber-Eins/mihomo from source**. It does not download a release
executable or require a binary copied into `/var/lib`.

## Build and check

```sh
nix build .#mihomo-smart
./result/bin/mihomo -v
nix flake check
```

For a Git checkout, new files must be tracked before Git-backed flake evaluation
can see them. In particular, include `nix/package.nix` and
`nix/smart-smoke.yaml` when committing changes to the flake.

The package:

- Uses the Go compiler from the locked nixpkgs revision.
- Builds only the main executable with `with_gvisor`, `CGO_ENABLED=0`, and the
  portable `GOAMD64=v1` baseline, rather than assuming amd64-v3 support.
- Fixes module dependencies with `vendorHash`; `go.mod` and `go.sum` remain the
  source of truth for Go dependency versions.
- Embeds the source revision and source timestamp, not the build machine's clock.
- Runs selected network-independent unit suites. The full upstream integration
  suite, which needs network access and other programs, is not claimed to run.
- Has a separate `smart-config` check that executes the built binary and parses
  a minimal `type: smart` configuration without remote providers or model data.

`packages.default` and `packages.mihomo-smart` refer to the same package.
For compatibility, `packages.mihomo-meta`, `overlay`, and `bin/mihomo-meta`
remain available; the primary executable is `bin/mihomo`.

The flake exports Linux and Darwin outputs for x86_64 and aarch64. A successful
build on one system is not evidence that all four were built and tested.

## NixOS integration

Add this fork as an input, and let the host control its nixpkgs revision:

```nix
{
  inputs.mihomo-smart.url = "github:Uber-Eins/mihomo/Alpha";
  inputs.mihomo-smart.inputs.nixpkgs.follows = "nixpkgs";
}
```

Pass `inputs` through `specialArgs`, then select the source-built package:

```nix
{ inputs, pkgs, ... }:
{
  services.mihomo.package =
    inputs.mihomo-smart.packages.${pkgs.stdenv.hostPlatform.system}.default;
}
```

This is a package override for an existing `services.mihomo` configuration, not
a complete service definition. A custom systemd service can instead use
`lib.getExe` on the same package while retaining its existing state directory
and security settings.

Alternatively, use `inputs.mihomo-smart.overlays.default` to provide
`pkgs.mihomo-smart` and the compatibility alias `pkgs.mihomo-meta`.

Keep production configuration, provider credentials, databases, and model
state outside the source tree and `/nix/store`. Build-time tests use the public
fixture in `nix/smart-smoke.yaml`, not a production configuration.

The host's `flake.lock` pins the fork revision. Publishing a packaging change
and updating that input is necessary before a host can reproduce a GitHub-based
build; a local `--override-input` used for development is not a published revision.

## Maintaining dependencies

To update the standalone toolchain:

```sh
nix flake update nixpkgs
nix flake check
```

Dependency and toolchain updates are automatic. When `go.mod`, `go.sum`,
`flake.lock`, `flake.nix`, or the packaging files change on `Alpha`, the
`Nix vendorHash` workflow runs `scripts/update-nix-vendorhash.sh`, which
discovers the module fixed-output hash and writes it to `nix/package.nix`. The
script only fetches the pinned `.#packages.<system>.default.goModules`; it does
not compile the binary or run tests. If the hash changed, the workflow commits
only `nix/package.nix` as a bot commit and pushes it to the triggering branch
without force. The workflow can also be started manually on any branch. Branch
settings must allow `github-actions[bot]` to push there; a protected branch
that rejects the push fails the workflow.

To refresh the hash locally, install Nix with flakes enabled and run:

```sh
sh scripts/update-nix-vendorhash.sh
```

The script first builds with `lib.fakeHash` to read the `got: sha256-…` value,
then verifies the real hash. On failure or interruption it restores the exact
previous `nix/package.nix`. This local packaging path is independent of the
OpenWrt build.

Do not set `vendorHash = null`, enable arbitrary network fetching during the
compile phase, or bypass `go.sum` verification to make a build pass.
