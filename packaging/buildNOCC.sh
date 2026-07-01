#!/bin/bash
#############################################################################
# buildNOCC.sh - Build nocc (VKCOM distributed C++ compiler) binaries.
#
# WHY THIS EXISTS / READ ME:
#   nocc is the FIRST FPP build/runtime dependency that is NOT a Debian
#   package -- everything else comes from apt. To keep that "new-ness"
#   contained, ALL of the from-source machinery lives in this one script:
#     * it runs on the BUILD HOST (never in the target image / under qemu),
#     * it bootstraps its OWN pinned Go toolchain into a host cache
#       (nothing Go-related ever lands in the image),
#     * it cross-compiles the three nocc binaries for the requested target,
#     * the image scripts (build-image-*.sh) just COPY the results into the
#       rootfs -- exactly like the pre-fetched kernel .deb.
#
#   nocc is what lets a painfully slow single-core board (BeagleBone Black,
#   Pi Zero W, ...) offload C++ compilation to a fast helper (a Pi 5) WITHOUT
#   pre-processing locally the way distcc does. See scripts/setup_nocc_host.sh
#   to stand up the helper, and docs/DeveloperNotes.md.
#
# WHAT IT PRODUCES (in --out):
#   nocc-daemon   pure Go, cross-compiled, statically linked (CGO disabled)
#   nocc-server   pure Go, cross-compiled, statically linked (CGO disabled)
#   nocc          ~300-line C++ launcher, cross-compiled with <triplet>-g++
#
# PROVENANCE / SUPPLY CHAIN (this is not a distro-vetted package):
#   NOCC_REF pins the upstream tag/commit; set NOCC_EXPECT_COMMIT to hard-fail
#   if the checkout doesn't match. GO_VERSION pins the toolchain; set
#   GO_SHA256 to verify the Go tarball. Bump these deliberately.
#
# Usage:
#   SD/buildNOCC.sh --arch <armhf|arm64|amd64> --out <dir> [--work <dir>] [--src <dir>]
#
#   --arch   TARGET architecture the binaries will run on (required)
#   --out    directory to write nocc / nocc-daemon / nocc-server into (required)
#   --work   host cache/scratch dir for Go + the nocc clone
#            (default: <out>/../nocc-build). Safe to keep between runs.
#   --src    build an EXISTING nocc checkout as-is instead of cloning+pinning.
#            The FalconChristmas/nocc fork's CI uses this: the fork IS the
#            pinned, org-controlled source, so there's nothing to clone/verify.
#
# Host prerequisites (a Debian/Ubuntu image-build host already has most):
#   wget, tar, git, and a C++ cross-compiler for the target triplet
#   (g++-arm-linux-gnueabihf / g++-aarch64-linux-gnu). When run as root the
#   cross-compiler is apt-installed automatically; otherwise install it first.
#############################################################################

set -euo pipefail

# ---- pinned versions (bump deliberately; this is a supply-chain input) ------
NOCC_REPO="${NOCC_REPO:-https://github.com/VKCOM/nocc.git}"
# Pin to a COMMIT, not a tag: upstream's newest tag (v1.2) lags master, and this
# is the master commit FPP actually validated on real BBB/Pi5 hardware. Bump
# deliberately and re-test. (nocc's own Makefile RELEASE string is "v1.2.2" but
# no such git tag exists.)
NOCC_REF="${NOCC_REF:-0bed389a3f6e3b60725fccf57a5857f887e944e1}"
NOCC_EXPECT_COMMIT="${NOCC_EXPECT_COMMIT:-0bed389a3f6e3b60725fccf57a5857f887e944e1}"
GO_VERSION="${GO_VERSION:-1.23.4}"
GO_SHA256="${GO_SHA256:-}"                  # optional: verify the Go tarball

ARCH=""
OUT=""
WORK=""
SRC_DIR=""

while [ $# -gt 0 ]; do
    case "$1" in
        --arch) ARCH="$2"; shift 2 ;;
        --out)  OUT="$2";  shift 2 ;;
        --work) WORK="$2"; shift 2 ;;
        --src)  SRC_DIR="$2"; shift 2 ;;
        -h|--help) sed -n '2,60p' "$0"; exit 0 ;;
        *) echo "buildNOCC: unknown option: $1" >&2; exit 2 ;;
    esac
done

[ -n "$ARCH" ] || { echo "buildNOCC: --arch is required (armhf|arm64|amd64)" >&2; exit 2; }
[ -n "$OUT" ]  || { echo "buildNOCC: --out is required" >&2; exit 2; }
mkdir -p "$OUT"; OUT="$(readlink -f "$OUT")"
WORK="${WORK:-$OUT/../nocc-build}"
mkdir -p "$WORK"; WORK="$(readlink -f "$WORK")"

# ---- map FPP arch -> Go target + Debian triplet -----------------------------
GOARCH=""; GOARM=""; TRIPLET=""
case "$ARCH" in
    armhf) GOARCH="arm";   GOARM="7"; TRIPLET="arm-linux-gnueabihf" ;;
    arm64) GOARCH="arm64";            TRIPLET="aarch64-linux-gnu" ;;
    amd64) GOARCH="amd64";            TRIPLET="x86_64-linux-gnu" ;;
    *) echo "buildNOCC: unsupported --arch '$ARCH' (armhf|arm64|amd64)" >&2; exit 2 ;;
esac

# ---- ensure host build tools (auto-install when run as root) -----------------
host_need() {  # <command> <apt-package>
    command -v "$1" >/dev/null 2>&1 && return 0
    if [ "$(id -u)" -eq 0 ]; then
        echo "==> Installing host build tool: $2"
        DEBIAN_FRONTEND=noninteractive apt-get install -y "$2" >/dev/null 2>&1 || true
    fi
    command -v "$1" >/dev/null 2>&1 || {
        echo "buildNOCC: missing host tool '$1' (install with: apt-get install $2)" >&2; exit 1; }
}
[ "$(id -u)" -eq 0 ] && DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates >/dev/null 2>&1 || true
host_need git git
host_need wget wget

# ---- pick the C++ cross-compiler for the tiny nocc launcher ------------------
# Only the launcher needs a compiler here, and it is a plain executable, so ANY
# version of the target-triplet g++ works (unlike the runtime version-match that
# nocc requires between a client and its helper). Prefer the triplet name; fall
# back to plain g++ only when building for the host's own arch.
CXX="${TRIPLET}-g++"
if ! command -v "$CXX" >/dev/null 2>&1; then
    if [ "$(id -u)" -eq 0 ]; then
        echo "==> Installing C++ cross-compiler g++-${TRIPLET}"
        DEBIAN_FRONTEND=noninteractive apt-get install -y "g++-${TRIPLET}" || true
    fi
fi
if ! command -v "$CXX" >/dev/null 2>&1; then
    HOSTARCH="$(dpkg --print-architecture 2>/dev/null || echo unknown)"
    if [ "$HOSTARCH" = "$ARCH" ] && command -v g++ >/dev/null 2>&1; then
        CXX="g++"   # native build for the host's own arch
    else
        echo "buildNOCC: need '${TRIPLET}-g++' to cross-build the nocc launcher." >&2
        echo "           Install it with:  apt-get install g++-${TRIPLET}" >&2
        exit 1
    fi
fi

# ---- bootstrap a pinned Go into the host cache (never enters the image) ------
HOSTM="$(uname -m)"
case "$HOSTM" in
    x86_64|amd64)  HOSTGOARCH="amd64" ;;
    aarch64|arm64) HOSTGOARCH="arm64" ;;
    armv7l|armhf)  HOSTGOARCH="armv6l" ;;
    *) echo "buildNOCC: unsupported build-host arch '$HOSTM' for Go bootstrap" >&2; exit 1 ;;
esac
GOROOT="$WORK/go"
if [ ! -x "$GOROOT/bin/go" ] || ! "$GOROOT/bin/go" version 2>/dev/null | grep -q "go${GO_VERSION} "; then
    GO_TGZ="go${GO_VERSION}.linux-${HOSTGOARCH}.tar.gz"
    echo "==> Fetching pinned Go toolchain ${GO_VERSION} (${HOSTGOARCH}) to host cache"
    wget -qO "$WORK/$GO_TGZ" "https://go.dev/dl/${GO_TGZ}"
    if [ -n "$GO_SHA256" ]; then
        echo "${GO_SHA256}  $WORK/$GO_TGZ" | sha256sum -c -
    fi
    rm -rf "$GOROOT"
    tar -C "$WORK" -xzf "$WORK/$GO_TGZ"
fi
export GOROOT
export PATH="$GOROOT/bin:$PATH"
export GOPATH="$WORK/gopath"
export GOCACHE="$WORK/gocache"
export GOMODCACHE="$WORK/gopath/pkg/mod"
export GOFLAGS="-buildvcs=false"
echo "==> Using $("$GOROOT/bin/go" version)"

# ---- obtain nocc source -----------------------------------------------------
# --src <dir> builds an existing checkout as-is (the FalconChristmas/nocc fork's
# CI uses this -- the fork IS the pinned, org-controlled source). Otherwise we
# clone + pin the upstream commit here.
if [ -n "$SRC_DIR" ]; then
    SRC="$(readlink -f "$SRC_DIR")"
    [ -f "$SRC/cmd/nocc.cpp" ] || { echo "buildNOCC: --src $SRC is not a nocc checkout" >&2; exit 1; }
    HEAD_COMMIT="$(git -C "$SRC" rev-parse HEAD 2>/dev/null || echo unknown)"
    echo "==> Building nocc from --src $SRC (${HEAD_COMMIT})"
else
    SRC="$WORK/nocc-src"
    if [ ! -d "$SRC/.git" ]; then
        echo "==> Cloning nocc ${NOCC_REF} from ${NOCC_REPO}"
        rm -rf "$SRC"
        git clone --quiet "$NOCC_REPO" "$SRC"
    fi
    git -C "$SRC" fetch --quiet --tags origin || true
    git -C "$SRC" checkout --quiet --force "$NOCC_REF"
    git -C "$SRC" reset --hard --quiet "$NOCC_REF" 2>/dev/null || true
    HEAD_COMMIT="$(git -C "$SRC" rev-parse HEAD)"
    echo "==> nocc @ ${NOCC_REF} (${HEAD_COMMIT})"
    if [ -n "$NOCC_EXPECT_COMMIT" ] && [ "$HEAD_COMMIT" != "$NOCC_EXPECT_COMMIT" ]; then
        echo "buildNOCC: commit mismatch! expected $NOCC_EXPECT_COMMIT got $HEAD_COMMIT" >&2
        exit 1
    fi
fi

# ---- build ------------------------------------------------------------------
VERSTR="${NOCC_REF} (${HEAD_COMMIT:0:12}, FPP)"
LDFLAGS="-s -w -X 'github.com/VKCOM/nocc/internal/common.version=${VERSTR}'"
GOENV=(env CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH")
[ -n "$GOARM" ] && GOENV+=(GOARM="$GOARM")

echo "==> Building nocc-daemon (Go, ${ARCH})"
"${GOENV[@]}" "$GOROOT/bin/go" build -C "$SRC" -trimpath -ldflags "$LDFLAGS" \
    -o "$OUT/nocc-daemon" cmd/nocc-daemon/main.go

echo "==> Building nocc-server (Go, ${ARCH})"
"${GOENV[@]}" "$GOROOT/bin/go" build -C "$SRC" -trimpath -ldflags "$LDFLAGS" \
    -o "$OUT/nocc-server" cmd/nocc-server/main.go

echo "==> Building nocc launcher (C++, ${ARCH}, via ${CXX})"
"$CXX" -std=c++11 -O3 "$SRC/cmd/nocc.cpp" -o "$OUT/nocc"

echo
echo "==> nocc binaries for ${ARCH} in ${OUT}:"
for b in nocc nocc-daemon nocc-server; do
    printf '    %-12s %s\n' "$b" "$(file -b "$OUT/$b" 2>/dev/null | cut -d, -f1-2)"
done
