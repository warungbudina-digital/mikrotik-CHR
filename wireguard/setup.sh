#!/bin/bash
set -euo pipefail

# =============================================================================
# Setup WireGuard server di VPS CHR (mikrotik-CHR stack)
# Jalankan sebagai root di VPS CHR.
#
# Sisi client (VPS Scraper / full-tool-browser) di-setup lewat script
# terpisah di repo full-tool-browser: wireguard/setup.sh — lihat README.md
# di folder ini.
# =============================================================================

WG_CONF="/etc/wireguard/wg0.conf"
KEY_DIR="/etc/wireguard"

# Idempotent: kalau wg0 sudah up dan sudah punya peer (scraper) terkonfigurasi,
# tidak perlu tanya ulang — ini yang bikin script ini aman dipanggil tanpa
# prompt dari installer utama (../setup.sh) di run berikutnya.
if wg show wg0 &>/dev/null && wg show wg0 | grep -q "peer:"; then
  echo "WireGuard wg0 sudah aktif dan terkonfigurasi — skip setup."
  wg show wg0
  exit 0
fi

echo "=== [1/6] Install WireGuard ==="
if ! command -v wg &>/dev/null; then
  apt-get update -qq
  apt-get install -y wireguard iptables-persistent
fi
mkdir -p "$KEY_DIR" && chmod 700 "$KEY_DIR"

echo "=== [2/6] Keypair ==="
if [[ -f "$KEY_DIR/privatekey" ]]; then
  echo "Keypair sudah ada, skip generate."
else
  wg genkey | tee "$KEY_DIR/privatekey" | wg pubkey > "$KEY_DIR/publickey"
fi
chmod 600 "$KEY_DIR/privatekey"

PRIVKEY=$(cat "$KEY_DIR/privatekey")
PUBKEY=$(cat "$KEY_DIR/publickey")

echo ""
echo "=== VPS CHR Keys ==="
echo "Public Key : $PUBKEY"
echo "(private key tidak ditampilkan — ada di $KEY_DIR/privatekey)"
echo ""

echo "=== [3/6] Deteksi interface & bridge Docker ==="
ETH_IFACE=$(ip route | awk '/default/ {print $5; exit}')
echo "Interface utama    : $ETH_IFACE"

BRIDGE_ID=$(docker network inspect mikrotik-net --format '{{.Id}}' 2>/dev/null | cut -c1-12 || true)
if [[ -z "$BRIDGE_ID" ]]; then
  echo "GAGAL deteksi bridge docker network 'mikrotik-net' — pastikan stack"
  echo "docker-compose CHR (mikrotik-CHR) sudah 'docker compose up -d' dulu."
  exit 1
fi
BRIDGE="br-$BRIDGE_ID"
echo "Bridge mikrotik-net: $BRIDGE"

PIHOLE_IP=$(docker inspect pihole --format '{{(index .NetworkSettings.Networks "mikrotik-net").IPAddress}}' 2>/dev/null || true)
MOSQUITTO_IP=$(docker inspect mosquitto --format '{{(index .NetworkSettings.Networks "mikrotik-net").IPAddress}}' 2>/dev/null || true)
if [[ -z "$PIHOLE_IP" || -z "$MOSQUITTO_IP" ]]; then
  echo "GAGAL deteksi IP container pihole/mosquitto di network mikrotik-net."
  echo "Cek nama network compose project ini pakai: docker inspect pihole --format '{{json .NetworkSettings.Networks}}'"
  exit 1
fi
echo "PiHole IP          : $PIHOLE_IP"
echo "Mosquitto IP       : $MOSQUITTO_IP"

echo ""
echo "=== [4/6] Input dari VPS Scraper ==="
read -rp "Public Key VPS Scraper: " SCRAPER_PUBKEY

echo "=== [5/6] Tulis $WG_CONF ==="
cat > "$WG_CONF" <<EOF
[Interface]
# VPS CHR — WireGuard server endpoint
Address = 10.10.0.1/24
ListenPort = 51820
PrivateKey = $PRIVKEY

PostUp   = sysctl -w net.ipv4.ip_forward=1
PostUp   = iptables -A FORWARD -i wg0 -j ACCEPT
PostUp   = iptables -A FORWARD -o wg0 -j ACCEPT
PostUp   = iptables -t nat -A POSTROUTING -s 10.10.0.0/24 -o $ETH_IFACE -j MASQUERADE
# Docker drops forwarded wg0->bridge traffic yang tidak match published-port
# rule miliknya sendiri — bypass di DOCKER-USER (dievaluasi sebelum
# DOCKER-FORWARD milik Docker). Lihat README.md di folder ini.
PostUp   = iptables -I DOCKER-USER 1 -i wg0 -o $BRIDGE -j ACCEPT
PostUp   = iptables -I DOCKER-USER 2 -i $BRIDGE -o wg0 -j ACCEPT
# MASQUERADE supaya reply dari container ter-translate balik lewat conntrack
PostUp   = iptables -t nat -A POSTROUTING -s 10.10.0.0/24 -d 172.20.0.0/24 -j MASQUERADE
# DNS dari VPS Scraper -> PiHole container
PostUp   = iptables -t nat -A PREROUTING -i wg0 -p udp --dport 53 -j DNAT --to-destination $PIHOLE_IP:53
PostUp   = iptables -t nat -A PREROUTING -i wg0 -p tcp --dport 53 -j DNAT --to-destination $PIHOLE_IP:53
# MQTT dari VPS Scraper -> Mosquitto container
PostUp   = iptables -t nat -A PREROUTING -i wg0 -p tcp --dport 1883 -j DNAT --to-destination $MOSQUITTO_IP:1883
PostUp   = iptables -t nat -A PREROUTING -i wg0 -p tcp --dport 9001 -j DNAT --to-destination $MOSQUITTO_IP:9001

PostDown = iptables -D FORWARD -i wg0 -j ACCEPT
PostDown = iptables -D FORWARD -o wg0 -j ACCEPT
PostDown = iptables -t nat -D POSTROUTING -s 10.10.0.0/24 -o $ETH_IFACE -j MASQUERADE
PostDown = iptables -D DOCKER-USER -i wg0 -o $BRIDGE -j ACCEPT
PostDown = iptables -D DOCKER-USER -i $BRIDGE -o wg0 -j ACCEPT
PostDown = iptables -t nat -D POSTROUTING -s 10.10.0.0/24 -d 172.20.0.0/24 -j MASQUERADE
PostDown = iptables -t nat -D PREROUTING -i wg0 -p udp --dport 53 -j DNAT --to-destination $PIHOLE_IP:53
PostDown = iptables -t nat -D PREROUTING -i wg0 -p tcp --dport 53 -j DNAT --to-destination $PIHOLE_IP:53
PostDown = iptables -t nat -D PREROUTING -i wg0 -p tcp --dport 1883 -j DNAT --to-destination $MOSQUITTO_IP:1883
PostDown = iptables -t nat -D PREROUTING -i wg0 -p tcp --dport 9001 -j DNAT --to-destination $MOSQUITTO_IP:9001

[Peer]
# VPS Scraper (full-tool-browser)
PublicKey = $SCRAPER_PUBKEY
AllowedIPs = 10.10.0.2/32
# PersistentKeepalive tidak diperlukan di server side
EOF
chmod 600 "$WG_CONF"

echo "=== [6/6] Enable & (re)start WireGuard ==="
systemctl enable wg-quick@wg0
systemctl restart wg-quick@wg0

echo ""
echo "=== Status WireGuard ==="
wg show

echo ""
echo "=== Simpan iptables rules agar persistent setelah reboot ==="
netfilter-persistent save

echo ""
echo "Setup VPS CHR selesai. Public Key: $PUBKEY"
