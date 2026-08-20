#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
PACKAGE_FILES_DIR="$PROJECT_DIR/packaging/openwrt"
OUTPUT_DIR=${MIHOMO_OUTPUT_DIR:-"$PROJECT_DIR/bin"}
PACKAGE_ARCH=${MIHOMO_PACKAGE_ARCH:-aarch64_cortex-a53}
CORE_VERSION=${MIHOMO_VERSION:-$(git -C "$PROJECT_DIR" rev-parse --short HEAD)}
PACKAGE_RELEASE=${MIHOMO_PACKAGE_RELEASE:-1}
PACKAGE_VERSION="$CORE_VERSION-$PACKAGE_RELEASE"
BUILD_TIME=${MIHOMO_BUILD_TIME:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}
BINARY_INPUT=${MIHOMO_BINARY:-}
PACKAGE="$OUTPUT_DIR/mihomo_${PACKAGE_VERSION}_${PACKAGE_ARCH}.ipk"

case "$CORE_VERSION$PACKAGE_RELEASE$PACKAGE_ARCH" in
	*[!A-Za-z0-9._+~-]*)
		echo "mihomo: version, release, and architecture must be package-safe strings" >&2
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

IPK_WORK_DIR=$(mktemp -d /tmp/mihomo-ipk.XXXXXX)
trap 'rm -rf "$IPK_WORK_DIR"' EXIT HUP INT TERM
DATA_DIR="$IPK_WORK_DIR/data"
CONTROL_DIR="$IPK_WORK_DIR/control"

install -d -m 0755 \
	"$DATA_DIR/usr/bin" \
	"$DATA_DIR/usr/share/mihomo" \
	"$DATA_DIR/etc/init.d" \
	"$CONTROL_DIR"

install -m 0755 "$BINARY" "$DATA_DIR/usr/bin/mihomo"
install -m 0755 "$PACKAGE_FILES_DIR/mihomo.init" "$DATA_DIR/etc/init.d/mihomo"
install -m 0644 "$PROJECT_DIR/.github/release/config.yaml" "$DATA_DIR/usr/share/mihomo/config.yaml.example"
install -m 0755 "$PACKAGE_FILES_DIR/postinst" "$CONTROL_DIR/postinst"
install -m 0755 "$PACKAGE_FILES_DIR/prerm" "$CONTROL_DIR/prerm"

(
	cd "$DATA_DIR"
	tar --format=gnu --numeric-owner --owner=0 --group=0 --sort=name -cf - .
) | gzip -n -9 > "$IPK_WORK_DIR/data.tar.gz"
chmod 0644 "$IPK_WORK_DIR/data.tar.gz"

INSTALLED_SIZE=$(stat -c '%s' "$IPK_WORK_DIR/data.tar.gz")
install -m 0644 "$PACKAGE_FILES_DIR/control.in" "$CONTROL_DIR/control"
sed -i \
	-e "s/@VERSION@/$PACKAGE_VERSION/g" \
	-e "s/@ARCHITECTURE@/$PACKAGE_ARCH/g" \
	-e "s/@INSTALLED_SIZE@/$INSTALLED_SIZE/g" \
	"$CONTROL_DIR/control"

(
	cd "$CONTROL_DIR"
	tar --format=gnu --numeric-owner --owner=0 --group=0 --sort=name -cf - .
) | gzip -n -9 > "$IPK_WORK_DIR/control.tar.gz"
chmod 0644 "$IPK_WORK_DIR/control.tar.gz"

install -m 0644 "$PACKAGE_FILES_DIR/debian-binary" "$IPK_WORK_DIR/debian-binary"
(
	cd "$IPK_WORK_DIR"
	tar --format=gnu --numeric-owner --owner=0 --group=0 --sort=name \
		-cf - ./debian-binary ./data.tar.gz ./control.tar.gz
) | gzip -n -9 > "$IPK_WORK_DIR/mihomo.ipk"

install -m 0644 "$IPK_WORK_DIR/mihomo.ipk" "$PACKAGE"
printf '%s\n' "$PACKAGE"
