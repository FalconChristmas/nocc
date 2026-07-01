#!/bin/bash
#############################################################################
# buildNOCC-deb.sh - Package nocc as a Debian .deb for the FPP apt repo.
#
# nocc is not in Debian, so FPP builds it from a pinned commit (buildNOCC.sh)
# and ships it through FPP's own apt repository. This script wraps the compiled
# binaries into a single "nocc" package containing:
#     /usr/bin/nocc  /usr/bin/nocc-daemon  /usr/bin/nocc-server
#     /lib/systemd/system/nocc-server.service   (shipped DISABLED)
#     /etc/default/nocc-server                  (conffile; edited by
#                                                scripts/setup_nocc_host.sh)
#
# Most devices only ever use the client (nocc + nocc-daemon); the server unit
# is inert until a helper is configured with setup_nocc_host.sh -- same model
# as the distcc package whose server ships disabled.
#
# Build the per-arch debs on a fast host (this is a CI / one-off job, NOT part
# of an image build), then publish them with SD/apt-repo.sh.
#
# Usage:
#   SD/buildNOCC-deb.sh --arch <armhf|arm64|amd64> --out <dir> \
#       [--work <dir>] [--version <debver>]
#############################################################################

set -euo pipefail

HERE="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"

ARCH=""; OUT=""; WORK=""; VERSION=""; SRC_DIR=""; BIN_DIR=""
while [ $# -gt 0 ]; do
    case "$1" in
        --arch)    ARCH="$2"; shift 2 ;;
        --out)     OUT="$2"; shift 2 ;;
        --work)    WORK="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --src)     SRC_DIR="$2"; shift 2 ;;   # build an existing checkout (the nocc fork's CI)
        --bin-dir) BIN_DIR="$2"; shift 2 ;;   # package pre-built nocc/-daemon/-server (skip build)
        -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
        *) echo "buildNOCC-deb: unknown option: $1" >&2; exit 2 ;;
    esac
done
[ -n "$ARCH" ] || { echo "buildNOCC-deb: --arch required (armhf|arm64|amd64)" >&2; exit 2; }
[ -n "$OUT" ]  || { echo "buildNOCC-deb: --out required" >&2; exit 2; }
case "$ARCH" in armhf|arm64|amd64) ;; *) echo "buildNOCC-deb: bad --arch $ARCH" >&2; exit 2;; esac
mkdir -p "$OUT"; OUT="$(readlink -f "$OUT")"
WORK="${WORK:-$OUT/nocc-build}"
mkdir -p "$WORK"; WORK="$(readlink -f "$WORK")"

command -v dpkg-deb >/dev/null 2>&1 || { echo "buildNOCC-deb: dpkg-deb not found (apt-get install dpkg-dev)" >&2; exit 1; }

# ---- 1. compile the binaries (pinned source + pinned Go, all in buildNOCC) ---
if [ -n "$BIN_DIR" ]; then
    # Package pre-built binaries (e.g. the armhf wrapper built with an ARMv6
    # toolchain in CI) instead of compiling here.
    BIN="$(readlink -f "$BIN_DIR")"
    for b in nocc nocc-daemon nocc-server; do
        [ -f "$BIN/$b" ] || { echo "buildNOCC-deb: --bin-dir missing $b" >&2; exit 1; }
    done
    echo "==> Packaging pre-built binaries from $BIN"
else
    BIN="$WORK/bin-$ARCH"
    rm -rf "$BIN"; mkdir -p "$BIN"
    "$HERE/buildNOCC.sh" --arch "$ARCH" --out "$BIN" --work "$WORK" ${SRC_DIR:+--src "$SRC_DIR"}
fi

# ---- 2. derive a Debian version from `git describe` -------------------------
# e.g. "v1.2-6-g0bed389" -> "1.2+6.g0bed389-1". The commit COUNT (6) is
# monotonic (every new commit bumps it, so apt always sees a newer build as an
# upgrade), the base tag auto-tracks upstream (merging a future v1.3 -> 1.3+N),
# and it's reproducible (same commit -> same version). Needs tags + history, so
# the CI checkout must use fetch-depth: 0.
SRCREF="${SRC_DIR:-$WORK/nocc-src}"
if [ -z "$VERSION" ]; then
    DESC="$(git -C "$SRCREF" describe --tags --long --always 2>/dev/null)"
    case "$DESC" in
        v*-*-g*)
            base="${DESC%%-*}"; base="${base#v}"   # v1.2-6-g0bed389 -> 1.2
            rest="${DESC#*-}"                        # 6-g0bed389
            VERSION="${base}+${rest%%-*}.${rest#*-}-1"   # 1.2+6.g0bed389-1
            ;;
        *)  # no tags reachable (shallow clone / detached) -- last-resort form
            SHA="$(git -C "$SRCREF" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
            echo "WARN: 'git describe' found no tag; version won't track upstream. Use fetch-depth: 0." >&2
            VERSION="0+git$(date -u +%Y%m%d).${SHA}-1"
            ;;
    esac
fi
echo "==> Packaging nocc ${VERSION} (${ARCH})"

# ---- 3. assemble the package tree -------------------------------------------
PKG="$WORK/pkg-$ARCH"
rm -rf "$PKG"
install -d "$PKG/DEBIAN" "$PKG/usr/bin" "$PKG/lib/systemd/system" "$PKG/etc/default" \
          "$PKG/usr/share/doc/nocc"
install -m 0755 "$BIN/nocc"        "$PKG/usr/bin/nocc"
install -m 0755 "$BIN/nocc-daemon" "$PKG/usr/bin/nocc-daemon"
install -m 0755 "$BIN/nocc-server" "$PKG/usr/bin/nocc-server"

cat > "$PKG/lib/systemd/system/nocc-server.service" <<'UNIT'
[Unit]
Description=nocc distributed C++ compile server (FPP)
Documentation=https://github.com/VKCOM/nocc
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/default/nocc-server
ExecStart=/usr/bin/nocc-server $NOCC_SERVER_OPTS
Restart=on-failure
RestartSec=2
Nice=10
# Transient unprivileged user + managed cache/log dirs (no user to create).
DynamicUser=yes
CacheDirectory=nocc
LogsDirectory=nocc

[Install]
WantedBy=multi-user.target
UNIT

cat > "$PKG/etc/default/nocc-server" <<'ENVF'
# Options for nocc-server (see `nocc-server -help`). Managed default; edited by
# scripts/setup_nocc_host.sh. SECURITY: nocc-server has NO access control --
# keep port 43210 restricted to your LAN (firewall it); never expose to WAN.
NOCC_SERVER_OPTS="-host 0.0.0.0 -port 43210 -cpp-dir /var/cache/nocc/cpp -obj-dir /var/cache/nocc/obj -log-filename /var/log/nocc/server.log -log-verbosity 0"
ENVF

# ---- 4. metadata + maintainer scripts --------------------------------------
INSTALLED_KB="$(du -ks "$PKG/usr" "$PKG/lib" "$PKG/etc" 2>/dev/null | awk '{s+=$1} END{print s}')"
cat > "$PKG/DEBIAN/control" <<CTRL
Package: nocc
Version: ${VERSION}
Architecture: ${ARCH}
Maintainer: FPP Project <fpp@falconchristmas.com>
Section: devel
Priority: optional
Depends: libc6, libstdc++6
Installed-Size: ${INSTALLED_KB:-0}
Homepage: https://github.com/VKCOM/nocc
Description: Distributed C++ compiler (VKCOM nocc), packaged for FPP
 nocc distributes C++ compilation to a remote helper WITHOUT preprocessing
 locally the way distcc does, letting slow single-core boards (BeagleBone
 Black, Pi Zero W, ...) offload builds to a fast helper (e.g. a Pi 5).
 .
 This package ships the client (nocc, nocc-daemon) plus the server
 (nocc-server, disabled by default -- enable a helper with
 scripts/setup_nocc_host.sh). Built by FPP from a pinned upstream commit.
CTRL

echo "/etc/default/nocc-server" > "$PKG/DEBIAN/conffiles"

cat > "$PKG/DEBIAN/postinst" <<'POST'
#!/bin/sh
set -e
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
exit 0
POST

cat > "$PKG/DEBIAN/prerm" <<'PRERM'
#!/bin/sh
set -e
if [ "$1" = remove ] || [ "$1" = deconfigure ]; then
    if [ -d /run/systemd/system ]; then
        systemctl stop nocc-server.service 2>/dev/null || true
    fi
fi
exit 0
PRERM

cat > "$PKG/DEBIAN/postrm" <<'POSTRM'
#!/bin/sh
set -e
if [ "$1" = purge ]; then
    rm -rf /var/cache/nocc /var/log/nocc
fi
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
exit 0
POSTRM
chmod 0755 "$PKG/DEBIAN/postinst" "$PKG/DEBIAN/prerm" "$PKG/DEBIAN/postrm"

cp "$SRCREF/LICENSE" "$PKG/usr/share/doc/nocc/copyright" 2>/dev/null || true

# ---- 5. build the .deb ------------------------------------------------------
DEB="$OUT/nocc_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group --build "$PKG" "$DEB"
echo
echo "==> Built: $DEB"
dpkg-deb -I "$DEB" | sed 's/^/    /'
