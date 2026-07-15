#!/usr/bin/env bash
#
# update-caddy.sh — Install / reinstall Caddy cleanly and obtain a TLS cert.
#
# Run AS ROOT on the VM (after gaining root via the Oracle serial-console
# rescue, then rebooting and `ssh root@<ip>`). Not for the init=/bin/bash
# rescue shell — systemd must be running.
#
# Env overrides (optional):
#   DOMAIN=4orge.duckdns.org  UPSTREAM=localhost:8080
#
set -euo pipefail

DOMAIN="${DOMAIN:-4orge.duckdns.org}"
UPSTREAM="${UPSTREAM:-localhost:8080}"
CADDY_BIN="/usr/bin/caddy"
CADDYFILE="/etc/caddy/Caddyfile"

# 1. Must be root.
if [ "$(id -u)" -ne 0 ]; then
  echo "ERROR: run this as root (su -  or  ssh root@<ip>). sudo is not required." >&2
  exit 1
fi

echo "==> Detecting architecture"
ARCH="$(uname -m)"
case "$ARCH" in
  aarch64|arm64) GOARCH="arm64" ;;
  x86_64|amd64)  GOARCH="amd64" ;;
  *) echo "ERROR: unsupported arch '$ARCH'" >&2; exit 1 ;;
esac
echo "    $ARCH -> $GOARCH"

# 2. DNS sanity check (the mistake that burned us last time).
echo "==> DNS sanity check"
HOST_IP="$(curl -fsS --max-time 10 https://api.ipify.org || true)"
RESOLVED="$(getent hosts "$DOMAIN" 2>/dev/null | awk '{print $1}' | head -1 || true)"
echo "    this host public IP : ${HOST_IP:-?}"
echo "    $DOMAIN resolves to : ${RESOLVED:-?}"
if [ -n "$HOST_IP" ] && [ "$HOST_IP" != "$RESOLVED" ]; then
  echo "WARNING: $DOMAIN does NOT point at this host. Cert issuance will FAIL" >&2
  echo "         until DuckDNS is updated to $HOST_IP." >&2
fi

# 3. Stop any existing Caddy (old deleted-binary process or a unit).
echo "==> Stopping any existing Caddy"
systemctl stop caddy 2>/dev/null || true
pkill -f 'caddy run' 2>/dev/null || true
sleep 2

# 4. Install the static binary from the official GitHub release (reliable, version-pinned).
CADDY_VERSION="${CADDY_VERSION:-2.8.4}"
CADDY_URL="https://github.com/caddyserver/caddy/releases/download/v${CADDY_VERSION}/caddy_${CADDY_VERSION}_linux_${GOARCH}.tar.gz"
echo "==> Installing Caddy binary (v${CADDY_VERSION}, ${GOARCH})"
curl -fsSL --max-time 120 "$CADDY_URL" -o /tmp/caddy.tgz
tar -xzf /tmp/caddy.tgz -C /tmp caddy
install -m 0755 /tmp/caddy "$CADDY_BIN"
rm -f /tmp/caddy.tgz
echo "    $("$CADDY_BIN" version | head -1)"

# 5. Write the Caddyfile.
echo "==> Writing $CADDYFILE"
mkdir -p /etc/caddy
cat > "$CADDYFILE" <<EOF
$DOMAIN {
    reverse_proxy $UPSTREAM
}
EOF
cat "$CADDYFILE"

# 6. Install a systemd unit (so it survives reboots this time).
echo "==> Installing systemd unit"
cat > /etc/systemd/system/caddy.service <<EOF
[Unit]
Description=Caddy
Documentation=https://caddyserver.com/docs/
After=network.target

[Service]
Type=notify
ExecStart=$CADDY_BIN run --environ --config $CADDYFILE --adapter caddyfile
ExecReload=$CADDY_BIN reload --config $CADDYFILE --adapter caddyfile
TimeoutStopSec=5s
LimitNOFILE=1048576
User=root
Group=root
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now caddy

# 7. Wait for the cert (Caddy obtains it via Let's Encrypt now that DNS is right).
echo "==> Waiting for cert (up to ~2 min)"
for i in $(seq 1 20); do
  sleep 6
  SUBJ="$(echo | openssl s_client -connect localhost:443 -servername "$DOMAIN" 2>/dev/null \
         | openssl x509 -noout -subject 2>/dev/null || true)"
  if [ -n "$SUBJ" ]; then
    echo "    cert obtained on attempt $i: $SUBJ"
    break
  fi
  echo "    attempt $i: cert not ready yet..."
done

# 8. Final verification.
echo "==> Verification"
echo "-- certificate --"
echo | openssl s_client -connect localhost:443 -servername "$DOMAIN" 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates 2>/dev/null || echo "    (no cert yet)"
echo "-- endpoint --"
curl -sS -o /dev/null -w "https=%{http_code}\n" "https://${DOMAIN}/jobs" 2>&1 || echo "    (curl failed)"
echo "==> Done."
