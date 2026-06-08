#!/usr/bin/env bash
# One-shot wrapper: build the image if needed, then run the booker once with
# the args you pass through. Example:
#   ./scripts/booker-run.sh -from 2026-09-22 -nights 1
#
# Used for production-style invocation (cron, prod container, etc.).
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE=schniffer-booker:dev
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    docker build -f docker/Dockerfile.booker -t "$IMAGE" .
fi

mkdir -p .cache/go-build .cache/go-mod .cache/recgov-profile .cache/debug

docker run --rm \
    --user "$(id -u):$(id -g)" \
    --shm-size=2g \
    -v "$PWD":/work \
    -v "$PWD/.cache/go-build":/work/.cache/go-build \
    -v "$PWD/.cache/go-mod":/go/pkg/mod \
    -v "$PWD/.cache/recgov-profile":/profile \
    -e HOME=/work/.cache \
    -e XDG_CONFIG_HOME=/work/.cache/.config \
    --env-file .env \
    "$IMAGE" \
    bash -c 'xvfb-run -a -s "-screen 0 1280x900x24" go run ./cmd/booker '"$*"
