#!/usr/bin/env bash
# Builds the uncompressed guest kernel (vmlinux) for fired sandboxes and
# writes it to an output directory. The recipe is the `kernel` stage of the
# Dockerfile (the fired image already contains this kernel at
# /data/kernels/vmlinux); use this to build one for a mounted kernels dir.
#
# Usage: scripts/build-kernel.sh [output-dir]      (needs Docker with BuildKit)
#   KERNEL_VERSION=6.1.140 FIRECRACKER_REF=v1.16.2 scripts/build-kernel.sh /mnt/hdd/fired/kernels
set -euo pipefail

OUT="${1:-$PWD/out}"
KERNEL_VERSION="${KERNEL_VERSION:-6.1.140}"
FIRECRACKER_REF="${FIRECRACKER_REF:-v1.16.2}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

mkdir -p "$OUT"
docker build --target kernel-export \
  --build-arg KERNEL_VERSION="$KERNEL_VERSION" --build-arg FIRECRACKER_VERSION="$FIRECRACKER_REF" \
  --output "type=local,dest=$OUT" "$ROOT"
echo "kernel written to $OUT/vmlinux"
