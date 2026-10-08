#!/bin/bash
# run.sh <label> <shim-bin-dir> <n> <every> [mode]  — isolated containerd as root with that shim first on PATH
set -euo pipefail
label=$1; shimdir=$2; n=$3; every=$4; mode=${5:-noclose}
T=${T:-$(cd "$(dirname "$0")/../.." && pwd)}; BIN=$T/bin-patched; ROOT=$T/run/$label; STATE=/run/ctrd-leak-$label; SOCK=$STATE/containerd.sock
mkdir -p "$ROOT"; rm -rf "$ROOT/root"; mkdir -p "$ROOT/root"
cat > "$ROOT/config.toml" <<CFG
version = 3
root = "$ROOT/root"
state = "$STATE"
disabled_plugins = ["io.containerd.cri.v1.runtime", "io.containerd.cri.v1.images", "io.containerd.grpc.v1.cri", "io.containerd.internal.v1.opt", "io.containerd.nri.v1.nri"]
[grpc]
  address = "$SOCK"
[ttrpc]
  address = "$SOCK.ttrpc"
[debug]
  address = "$STATE/debug.sock"
  level = "info"
CFG
export PATH="$shimdir:$BIN:/usr/sbin:/usr/bin:/sbin:/bin"
echo "== $label: shim $(ls -l "$shimdir/containerd-shim-runc-v2" | awk '{print $NF, $5}') runc=$(command -v runc)"
"$BIN/containerd" --config "$ROOT/config.toml" > "$ROOT/containerd.log" 2>&1 &
cpid=$!
for i in $(seq 1 50); do [ -S "$SOCK" ] && break; sleep 0.2; done
[ -S "$SOCK" ] || { echo "containerd did not start"; tail -20 "$ROOT/containerd.log"; kill $cpid; exit 1; }
"$BIN/leakdrv" -address "$SOCK" -n "$n" -every "$every" -mode "$mode" -log "$ROOT/containerd.log" || true
kill $cpid; wait $cpid 2>/dev/null || true
grep -ciE "level=(error|fatal)" "$ROOT/containerd.log" | sed 's/^/containerd error lines: /'
