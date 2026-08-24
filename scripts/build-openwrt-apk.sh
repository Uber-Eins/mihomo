#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
PACKAGE_FILES_DIR="$PROJECT_DIR/packaging/openwrt"
OUTPUT_DIR=${MIHOMO_OUTPUT_DIR:-"$PROJECT_DIR/bin"}
PACKAGE_ARCH=${MIHOMO_PACKAGE_ARCH:-aarch64_cortex-a53}
CORE_VERSION=${MIHOMO_VERSION:-$(git -C "$PROJECT_DIR" rev-parse --short HEAD)}
PACKAGE_RELEASE=${MIHOMO_PACKAGE_RELEASE:-1}
BUILD_TIME=${MIHOMO_BUILD_TIME:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}
SOURCE_DATE_EPOCH=${MIHOMO_SOURCE_DATE_EPOCH:-$(git -C "$PROJECT_DIR" show -s --format=%ct HEAD)}
SOURCE_REVISION=${MIHOMO_SOURCE_REVISION:-$(git -C "$PROJECT_DIR" rev-parse --short HEAD)}
APK_BIN=${MIHOMO_APK_BIN:-apk}
APK_IMAGE=${MIHOMO_APK_IMAGE:-alpine:3.23}
BINARY_INPUT=${MIHOMO_BINARY:-}

case "$PACKAGE_RELEASE" in
	''|*[!0-9]*)
		echo "mihomo: package release must be a non-negative integer" >&2
		exit 1
		;;
esac

case "$CORE_VERSION$PACKAGE_ARCH$SOURCE_REVISION" in
	*[!A-Za-z0-9._+~-]*)
		echo "mihomo: core version, architecture, and revision must be package-safe strings" >&2
		exit 1
		;;
esac

if [ -n "${MIHOMO_APK_VERSION:-}" ]; then
	PACKAGE_VERSION=$MIHOMO_APK_VERSION
else
	case "$CORE_VERSION" in
		v[0-9]*) PACKAGE_BASE_VERSION=${CORE_VERSION#v} ;;
		[0-9]*) PACKAGE_BASE_VERSION=$CORE_VERSION ;;
		*) PACKAGE_BASE_VERSION="${SOURCE_DATE_EPOCH}~${SOURCE_REVISION}" ;;
	esac
	case "$PACKAGE_BASE_VERSION" in
		''|*[!0-9a-z._~]*|*-*)
			PACKAGE_BASE_VERSION="${SOURCE_DATE_EPOCH}~${SOURCE_REVISION}"
			;;
	esac
	PACKAGE_VERSION="${PACKAGE_BASE_VERSION}-r${PACKAGE_RELEASE}"
fi

case "$PACKAGE_VERSION" in
	''|*[!A-Za-z0-9._+~-]*)
		echo "mihomo: APK version must be a package-safe string" >&2
		exit 1
		;;
esac

mkdir -p "$OUTPUT_DIR"

if [ -n "$BINARY_INPUT" ]; then
	[ -f "$BINARY_INPUT" ] || {
		echo "mihomo: binary not found: $BINARY_INPUT" >&2
		exit 1
	}
	BINARY=$BINARY_INPUT
else
	BINARY="$OUTPUT_DIR/mihomo-linux-arm64"
	LDFLAGS="-X github.com/metacubex/mihomo/constant.Version=$CORE_VERSION -X github.com/metacubex/mihomo/constant.BuildTime=$BUILD_TIME -w -s -buildid="
	(
		cd "$PROJECT_DIR"
		CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOARM64=v8.0 \
			go build -tags with_gvisor -trimpath -ldflags "$LDFLAGS" -o "$BINARY" .
	)
	chmod 0755 "$BINARY"
fi

APK_WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/mihomo-apk.XXXXXX")
trap 'rm -rf "$APK_WORK_DIR"' EXIT HUP INT TERM
DATA_DIR="$APK_WORK_DIR/data"
PACKAGE="$OUTPUT_DIR/mihomo-${PACKAGE_VERSION}.apk"

install -d -m 0755 \
	"$DATA_DIR/usr/bin" \
	"$DATA_DIR/usr/share/mihomo" \
	"$DATA_DIR/lib/apk/packages"

install -m 0755 "$BINARY" "$DATA_DIR/usr/bin/mihomo"
install -m 0755 "$PACKAGE_FILES_DIR/mihomo.init" "$DATA_DIR/usr/share/mihomo/mihomo.init"
install -m 0644 "$PROJECT_DIR/.github/release/config.yaml" "$DATA_DIR/usr/share/mihomo/config.yaml.example"

(
	cd "$DATA_DIR"
	find . \( -type f -o -type l \) -print | sed 's#^\./#/#' | LC_ALL=C sort
) > "$APK_WORK_DIR/mihomo.list"
install -m 0644 "$APK_WORK_DIR/mihomo.list" "$DATA_DIR/lib/apk/packages/mihomo.list"

package_with_apk() {
	SOURCE_DATE_EPOCH=0 "$@" mkpkg \
		--info "name:mihomo" \
		--info "version:$PACKAGE_VERSION" \
		--info "tags:openwrt:section=net" \
		--info "description:Mihomo proxy platform with an optional OpenWrt procd init script" \
		--info "arch:$PACKAGE_ARCH" \
		--info "license:GPL-3.0-or-later" \
		--info "origin:mihomo" \
		--info "url:https://wiki.metacubex.one/" \
		--info "maintainer:MetaCubeX" \
		--info "depends:procd" \
		--script "post-install:$PACKAGE_FILES_DIR/postinst" \
		--script "post-upgrade:$PACKAGE_FILES_DIR/postinst" \
		--files "$DATA_DIR" \
		--output "$APK_WORK_DIR/mihomo.apk"
}

if command -v "$APK_BIN" >/dev/null 2>&1 && "$APK_BIN" mkpkg --help >/dev/null 2>&1; then
	"$APK_BIN" version --check "$PACKAGE_VERSION" >/dev/null
	if [ "$(id -u)" -eq 0 ]; then
		package_with_apk "$APK_BIN"
	elif command -v fakeroot >/dev/null 2>&1; then
		package_with_apk fakeroot "$APK_BIN"
	else
		echo "mihomo: fakeroot is required when apk mkpkg is run as a non-root user" >&2
		exit 1
	fi
else
	if [ -n "${MIHOMO_CONTAINER:-}" ]; then
		CONTAINER_BIN=$MIHOMO_CONTAINER
	elif command -v docker >/dev/null 2>&1; then
		CONTAINER_BIN=docker
	elif command -v podman >/dev/null 2>&1; then
		CONTAINER_BIN=podman
	else
		echo "mihomo: apk-tools v3, Docker, or Podman is required to build an APK package" >&2
		exit 1
	fi

	"$CONTAINER_BIN" run --rm \
		-e "PACKAGE_VERSION=$PACKAGE_VERSION" \
		-e "PACKAGE_ARCH=$PACKAGE_ARCH" \
		-v "$APK_WORK_DIR:/work" \
		-v "$PACKAGE_FILES_DIR:/package-files:ro" \
		"$APK_IMAGE" sh -eu -c '
			cp -a /work/data /tmp/data
			chown -R 0:0 /tmp/data
			apk version --check "$PACKAGE_VERSION" >/dev/null
			SOURCE_DATE_EPOCH=0 apk mkpkg \
				--info "name:mihomo" \
				--info "version:$PACKAGE_VERSION" \
				--info "tags:openwrt:section=net" \
				--info "description:Mihomo proxy platform with an optional OpenWrt procd init script" \
				--info "arch:$PACKAGE_ARCH" \
				--info "license:GPL-3.0-or-later" \
				--info "origin:mihomo" \
				--info "url:https://wiki.metacubex.one/" \
				--info "maintainer:MetaCubeX" \
				--info "depends:procd" \
				--script "post-install:/package-files/postinst" \
				--script "post-upgrade:/package-files/postinst" \
				--files /tmp/data \
				--output /work/mihomo.apk
		'
fi

install -m 0644 "$APK_WORK_DIR/mihomo.apk" "$PACKAGE"
printf '%s\n' "$PACKAGE"
