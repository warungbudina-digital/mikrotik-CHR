#!/bin/bash
set -euo pipefail

# =============================================================================
# Setup WireGuard di VPS CHR (server side)
# Jalankan sebagai root di VPS CHR
# =============================================================================

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WG_CONF="/etc/wireguard/wg0.conf"
KEY_DIR="/etc/wireguard"

echo "=== [1/5] Install WireGuard ==="
apt-get update -qq
apt-get install -y wireguard iptables-persistent

echo "=== [2/5] Generate keypair ==="
if [[ -f "$KEY_DIR/privatekey" ]]; then
  echo "Keypair sudah ada, skip generate."
else
  wg genkey | tee "$KEY_DIR/privatekey" | wg pubkey > "$KEY_DIR/publickey"
  chmod 600 "$KEY_DIR/privatekey"
fi

PRIVKEY=$(cat "$KEY_DIR/privatekey")
PUBKEY=$(cat "$KEY_DIR/publickey")

echo ""
echo "=== VPS CHR Keys ==="
echo "Private Key : $PRIVKEY  (jangan dibagikan)"
echo "Public Key  : $PUBKEY   (berikan ke VPS Scraper)"
echo ""

echo "=== [3/5] Detect network interface ==="
ETH_IFACE=$(ip route | awk '/default/ {print $5; exit}')
echo "Interface utama: $ETH_IFACE"

echo "=== [4/5] Install konfigurasi WireGuard ==="
cp "$REPO_DIR/wireguard/wg0.conf" "$WG_CONF"
sed -i "s|REPLACE_WITH_CHR_PRIVATE_KEY|$PRIVKEY|g" "$WG_CONF"
sed -i "s|ETH_IFACE|$ETH_IFACE|g" "$WG_CONF"

echo ""
echo "PERHATIAN: Edit $WG_CONF dan isi REPLACE_WITH_SCRAPER_PUBLIC_KEY"
echo "dengan public key dari VPS Scraper sebelum lanjut."
echo ""
read -rp "Sudah diisi? (y/N) " confirm
[[ "$confirm" =~ ^[Yy]$ ]] || { echo "Aborted."; exit 1; }

echo "=== [5/5] Enable & start WireGuard ==="
systemctl enable wg-quick@wg0
systemctl start wg-quick@wg0

echo ""
echo "=== Status WireGuard ==="
wg show

echo ""
echo "=== Simpan iptables rules agar persistent setelah reboot ==="
netfilter-persistent save

echo ""
echo "Setup VPS CHR selesai!"
echo "Public Key CHR: $PUBKEY"
