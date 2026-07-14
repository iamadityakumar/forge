#!/usr/bin/env bash
# Run this ON the Oracle Cloud ARM VM (Ubuntu 22.04/24.04) as a user with sudo.
#
# What it does:
#   1. Installs Docker + the docker compose plugin (if missing).
#   2. Clones the Forge repo.
#   3. Builds the image natively for linux/arm64 and starts the stack.
#   4. Verifies POST /jobs returns a job id.
#
# Usage:
#   REPO_URL=https://github.com/<you>/forge.git bash setup-vm.sh
set -euo pipefail

REPO_URL="${REPO_URL:?set REPO_URL, e.g. https://github.com/you/forge.git}"
APP_DIR="${APP_DIR:-$HOME/forge}"

if [ "$(id -u)" -eq 0 ]; then
  echo "Do not run this as root; run as the default (ubuntu) user with sudo." >&2
  exit 1
fi

echo "==> Installing Docker (if missing)"
if ! command -v docker >/dev/null 2>&1; then
  sudo apt-get update -y
  sudo apt-get install -y ca-certificates curl gnupg lsb-release
  sudo install -m 0755 -d /etc/apt/keyrings
  sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  sudo chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
    https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
    | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
  sudo apt-get update -y
  sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
  sudo systemctl enable --now docker
  sudo usermod -aG docker "$USER"
  echo "NOTE: log out and back in (or run 'newgrp docker') so docker works without sudo."
fi

echo "==> Cloning repo into $APP_DIR"
if [ ! -d "$APP_DIR" ]; then
  git clone "$REPO_URL" "$APP_DIR"
fi
cd "$APP_DIR"

echo "==> Building & starting stack (arm64)"
docker compose up -d --build

echo "==> Verifying API"
sleep 3
curl -sS -X POST http://localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{"task":"ping"}'
echo
echo "==> Done. Check a job with: curl -sS http://localhost:8080/jobs/<job_id>"
