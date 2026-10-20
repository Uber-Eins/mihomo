#!/bin/sh
# Refresh vendorHash in nix/package.nix from the pinned flake's Go module
# derivation. Only the module fetch runs; the executable and tests are not built.

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
PACKAGE_FILE="$PROJECT_DIR/nix/package.nix"
WORK_DIR=
BACKUP=

fail () {
	echo "mihomo: $*" >&2
	exit 1
}

# Any non-zero exit (including signals) puts back the original package file.
# A successful run exits 0 after writing the discovered hash, so it is kept.
cleanup () {
	status=$?
	trap - EXIT HUP INT TERM
	if [ "$status" -ne 0 ] && [ -n "$BACKUP" ]; then
		if cp "$BACKUP" "$PACKAGE_FILE"; then
			echo "mihomo: restored $PACKAGE_FILE" >&2
		else
			echo "mihomo: could not restore $PACKAGE_FILE; original kept at $BACKUP" >&2
			exit "$status"
		fi
	fi
	if [ -n "$WORK_DIR" ]; then
		rm -rf "$WORK_DIR"
	fi
	exit "$status"
}

[ -f "$PACKAGE_FILE" ] || fail "nix/package.nix not found"
command -v nix >/dev/null 2>&1 || fail "nix is required"

vendor_lines=$(grep -c '^  vendorHash = .*;$' "$PACKAGE_FILE" || true)
[ "$vendor_lines" = 1 ] || fail "expected exactly one vendorHash line in nix/package.nix"

cd "$PROJECT_DIR"
system=$(nix eval --impure --raw --expr builtins.currentSystem)
go_modules_attr=".#packages.$system.default.goModules"

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/mihomo-vendorhash.XXXXXX")
LOG="$WORK_DIR/nix-build.log"
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 129' HUP
trap 'exit 143' TERM

cp "$PACKAGE_FILE" "$WORK_DIR/package.nix.orig"
BACKUP="$WORK_DIR/package.nix.orig"

build_go_modules () {
	nix build --no-link --no-update-lock-file "$go_modules_attr" >"$LOG" 2>&1
}

sed 's|^  vendorHash = .*;$|  vendorHash = lib.fakeHash;|' "$BACKUP" >"$WORK_DIR/fake.nix"
cat "$WORK_DIR/fake.nix" >"$PACKAGE_FILE"

if build_go_modules; then
	fail "goModules built with lib.fakeHash; cannot discover vendorHash"
fi

hashes=$(sed -n 's/.*got: *\(sha256-[A-Za-z0-9+/]\{43\}=\).*/\1/p' "$LOG" | sort -u)
if [ "$(printf '%s\n' "$hashes" | grep -c '^sha256-')" != 1 ]; then
	tail -n 40 "$LOG" >&2
	fail "could not parse one 'got: sha256-…' vendorHash from nix output (network or build error?)"
fi
hash=$hashes

sed "s|^  vendorHash = .*;\$|  vendorHash = \"$hash\";|" "$BACKUP" >"$WORK_DIR/updated.nix"
cat "$WORK_DIR/updated.nix" >"$PACKAGE_FILE"

if ! build_go_modules; then
	tail -n 40 "$LOG" >&2
	fail "goModules failed with discovered vendorHash $hash"
fi

echo "mihomo: vendorHash in nix/package.nix is $hash"
