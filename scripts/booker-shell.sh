#!/usr/bin/env bash
# Open an interactive shell inside the booker dev container with the repo,
# Go caches, and Chrome profile bind-mounted. Inside, run the booker like:
#
#   xvfb-run -a -s "-screen 0 1280x900x24" \
#     go run ./cmd/booker -from 2026-09-22 -nights 1
#
# UID:GID maps to host 1000:1000 so files created in .cache/ stay owned by the
# host user.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE=schniffer-booker:dev
docker build -f docker/Dockerfile.booker -t "$IMAGE" .

mkdir -p .cache/go-build .cache/go-mod .cache/recgov-profile .cache/debug

docker run --rm -it \
    --name booker-dev \
    --user "$(id -u):$(id -g)" \
    --shm-size=2g \
    -v "$PWD":/work \
    -v "$PWD/.cache/go-build":/work/.cache/go-build \
    -v "$PWD/.cache/go-mod":/go/pkg/mod \
    -v "$PWD/.cache/recgov-profile":/profile \
    -e HOME=/work/.cache \
    -e XDG_CONFIG_HOME=/work/.cache/.config \
    --env-file .env \
    "$IMAGE" bash
