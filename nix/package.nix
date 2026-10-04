{
  lib,
  buildGoModule,
  version ? "alpha-smart-dev",
  buildTime ? "unknown",
}:

buildGoModule {
  pname = "mihomo-smart";
  inherit version;
  src = lib.cleanSource ../.;

  # Update this hash when go.mod/go.sum change; never replace with vendorHash=null.
  vendorHash = "sha256-njcAhz2DY5pwfL4NyxwXyLyhZ9SGSXuk18ojrZg0SSA=";
  subPackages = [ "." ];

  env = {
    CGO_ENABLED = "0";
    GOTOOLCHAIN = "local";
    # Unlike the release Makefile's amd64-v3 default, this also runs on Pentiums.
    GOAMD64 = "v1";
  };
  tags = [ "with_gvisor" ];
  ldflags = [
    "-s"
    "-w"
    "-X github.com/metacubex/mihomo/constant.Version=${version}"
    "-X github.com/metacubex/mihomo/constant.BuildTime=${buildTime}"
  ];

  # The full upstream suite includes network/interop tests. These pure unit
  # suites are safe inside the Nix sandbox; smart parsing is a separate flake check.
  doCheck = true;
  checkPhase = ''
    runHook preCheck
    go test -tags=with_gvisor -count=1 \
      ./common/structure ./common/convert ./common/orderedmap ./component/trie
    runHook postCheck
  '';

  postInstall = ''
    ln -s mihomo "$out/bin/mihomo-meta"
  '';

  meta = {
    description = "Uber-Eins Mihomo fork with smart proxy groups and gVisor support";
    homepage = "https://github.com/Uber-Eins/mihomo";
    license = lib.licenses.gpl3Only;
    mainProgram = "mihomo";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}
