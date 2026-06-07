#!/usr/bin/env bash
# Install google-chrome-stable and the shared libs chromedp needs.
# Works on Debian/Ubuntu (WSL Ubuntu, Debian bookworm Docker base, etc.).
set -euo pipefail

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
    SUDO="sudo"
fi

export DEBIAN_FRONTEND=noninteractive

$SUDO apt-get update

# Chrome runtime deps. libasound2t64 on Ubuntu 24.04+, libasound2 on older.
ASOUND_PKG="libasound2t64"
if ! apt-cache show "$ASOUND_PKG" >/dev/null 2>&1; then
    ASOUND_PKG="libasound2"
fi

$SUDO apt-get install -y \
    wget gnupg ca-certificates fonts-liberation xdg-utils \
    libnss3 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 libxkbcommon0 \
    libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 \
    libpango-1.0-0 libcairo2 "$ASOUND_PKG"

# Google's APT repo (idempotent).
KEYRING=/usr/share/keyrings/google-chrome.gpg
if [ ! -f "$KEYRING" ]; then
    wget -qO- https://dl.google.com/linux/linux_signing_key.pub \
        | $SUDO gpg --dearmor -o "$KEYRING"
fi

LIST=/etc/apt/sources.list.d/google-chrome.list
if [ ! -f "$LIST" ]; then
    echo "deb [arch=amd64 signed-by=$KEYRING] http://dl.google.com/linux/chrome/deb/ stable main" \
        | $SUDO tee "$LIST" >/dev/null
fi

$SUDO apt-get update
$SUDO apt-get install -y google-chrome-stable

echo
echo "Installed:"
google-chrome-stable --version
echo "Path: $(command -v google-chrome-stable)"
