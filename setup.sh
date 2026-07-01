#!/bin/bash
# =============================================================================
# setup.sh — One-shot installer untuk VPS CHR (mikrotik-CHR project)
# Prasyarat: Ubuntu 22.04+ dengan Docker sudah terinstall
# Jalankan sebagai root: sudo bash setup.sh
# =============================================================================
set -euo pipefail

# ─────────────────────────────────────────────
# Warna & helper
# ─────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()      { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
header()  { echo -e "\n${BOLD}${CYAN}══════ $* ══════${NC}\n"; }
die()     { error "$*"; exit 1; }

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ─────────────────────────────────────────────
# [0] Preflight checks
# ─────────────────────────────────────────────
header "Preflight Checks"

[[ $EUID -eq 0 ]] || die "Script harus dijalankan sebagai root: sudo bash setup.sh"

# Deteksi OS
if [[ -f /etc/os-release ]]; then
  source /etc/os-release
  info "OS: $PRETTY_NAME"
  [[ "$ID" == "ubuntu" || "$ID" == "debian" || "$ID_LIKE" == *"debian"* ]] \
    || warn "Script dioptimasi untuk Ubuntu/Debian. OS lain mungkin perlu penyesuaian."
else
  warn "Tidak bisa deteksi OS — lanjut dengan asumsi Debian-based"
fi

# Docker
if command -v docker &>/dev/null; then
  DOCKER_VER=$(docker --version | awk '{print $3}' | tr -d ',')
  ok "Docker $DOCKER_VER ditemukan"
else
  die "Docker tidak terinstall. Install dulu: https://docs.docker.com/engine/install/"
fi

# Docker Compose (plugin v2 atau standalone v1)
if docker compose version &>/dev/null 2>&1; then
  COMPOSE_VER=$(docker compose version --short 2>/dev/null || echo "v2")
  ok "Docker Compose $COMPOSE_VER ditemukan"
elif command -v docker-compose &>/dev/null; then
  warn "docker-compose v1 ditemukan — disarankan upgrade ke Docker Compose Plugin v2"
  COMPOSE_CMD="docker-compose"
else
  die "Docker Compose tidak ditemukan. Install: apt install docker-compose-plugin"
fi
COMPOSE_CMD="${COMPOSE_CMD:-docker compose}"

# ─────────────────────────────────────────────
# [1] Install host dependencies
# ─────────────────────────────────────────────
header "Install Host Dependencies"

PKGS_NEEDED=()
for pkg in wireguard wireguard-tools iptables-persistent netfilter-persistent curl jq git; do
  if ! dpkg -s "$pkg" &>/dev/null 2>&1; then
    PKGS_NEEDED+=("$pkg")
  else
    ok "$pkg sudah terinstall"
  fi
done

if [[ ${#PKGS_NEEDED[@]} -gt 0 ]]; then
  info "Menginstall: ${PKGS_NEEDED[*]}"
  apt-get update -qq
  # iptables-persistent akan menanyakan apakah save rules saat install — jawab no
  DEBIAN_FRONTEND=noninteractive apt-get install -y "${PKGS_NEEDED[@]}"
  ok "Semua package berhasil diinstall"
fi

# Aktifkan IP forwarding (persistent)
if ! grep -q "^net.ipv4.ip_forward=1" /etc/sysctl.conf 2>/dev/null; then
  echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
  sysctl -w net.ipv4.ip_forward=1 -q
  ok "IP forwarding diaktifkan (persistent)"
else
  ok "IP forwarding sudah aktif"
fi

# ─────────────────────────────────────────────
# [2] Setup file .env
# ─────────────────────────────────────────────
header "Konfigurasi Environment (.env)"

ENV_FILE="$REPO_DIR/.env"
ENV_EXAMPLE="$REPO_DIR/.env.example"

if [[ -f "$ENV_FILE" ]]; then
  ok ".env sudah ada — skip (hapus jika ingin konfigurasi ulang)"
else
  info "Membuat .env dari .env.example..."
  cp "$ENV_EXAMPLE" "$ENV_FILE"

  echo ""
  echo -e "${BOLD}Isi nilai berikut (Enter = skip / gunakan placeholder):${NC}"
  echo ""

  # Helper: prompt dan replace di .env
  prompt_replace() {
    local key="$1" prompt="$2" default="$3" secret="${4:-no}"
    if [[ "$secret" == "yes" ]]; then
      read -rsp "  $prompt: " val; echo ""
    else
      read -rp  "  $prompt [$default]: " val
    fi
    val="${val:-$default}"
    # Escape untuk sed
    local escaped
    escaped=$(printf '%s\n' "$val" | sed 's/[[\.*^$()+?{|]/\\&/g')
    sed -i "s|^${key}=.*|${key}=${escaped}|" "$ENV_FILE"
  }

  prompt_replace "CLOUDFLARE_TUNNEL_TOKEN" "Cloudflare Tunnel Token" "your_cloudflare_tunnel_token_here"
  prompt_replace "PIHOLE_PASSWORD" "PiHole Admin Password" "$(openssl rand -base64 12)" "yes"
  prompt_replace "MQTT_USERNAME" "MQTT Username" "browser-agent"
  prompt_replace "MQTT_PASSWORD" "MQTT Password" "$(openssl rand -base64 16)" "yes"
  prompt_replace "JWT_SECRET" "JWT Secret (kosong = auto-generate)" "$(openssl rand -hex 64)"

  ok ".env berhasil dibuat"
  echo ""
  warn "Simpan credentials di tempat aman — tidak bisa direcovery setelah ini"
fi

# Load env untuk dipakai script ini
set -a; source "$ENV_FILE"; set +a

# ─────────────────────────────────────────────
# [3] Generate Mosquitto password file
# ─────────────────────────────────────────────
header "MQTT — Mosquitto Password File"

PASSWD_FILE="$REPO_DIR/mosquitto/passwd"

if [[ -f "$PASSWD_FILE" ]]; then
  ok "Mosquitto password file sudah ada — skip"
else
  info "Generate Mosquitto password file menggunakan Docker..."

  MQTT_USER="${MQTT_USERNAME:-browser-agent}"
  MQTT_PASS="${MQTT_PASSWORD:-}"

  if [[ -z "$MQTT_PASS" ]]; then
    read -rsp "  MQTT Password untuk '$MQTT_USER': " MQTT_PASS; echo ""
    read -rsp "  Konfirmasi: " MQTT_PASS2; echo ""
    [[ "$MQTT_PASS" == "$MQTT_PASS2" ]] || die "Password tidak cocok"
  fi

  docker run --rm \
    -v "$REPO_DIR/mosquitto:/passwd_dir" \
    eclipse-mosquitto:2 \
    sh -c "mosquitto_passwd -b -c /passwd_dir/passwd '$MQTT_USER' '$MQTT_PASS'" \
    && ok "Mosquitto password file dibuat untuk user: $MQTT_USER" \
    || die "Gagal membuat Mosquitto password file"
fi

# ─────────────────────────────────────────────
# [4] Setup WireGuard (host)
# ─────────────────────────────────────────────
header "WireGuard Setup (Host)"

# wireguard/setup.sh sudah idempotent — skip sendiri (tanpa prompt) kalau wg0
# sudah aktif & terkonfigurasi, jadi aman dipanggil tiap kali installer ini jalan.
info "Menjalankan wireguard/setup.sh..."
bash "$REPO_DIR/wireguard/setup.sh"
# (wireguard/setup.sh sudah handle systemctl enable + netfilter-persistent save sendiri)

# ─────────────────────────────────────────────
# [5] Docker Compose — Build & Up
# ─────────────────────────────────────────────
header "Docker Compose — Build & Start"

cd "$REPO_DIR"

info "Build images (grpc-server)..."
$COMPOSE_CMD build --quiet grpc-server \
  && ok "Build selesai" \
  || die "docker compose build gagal"

info "Start semua services..."
$COMPOSE_CMD up -d --remove-orphans \
  && ok "Semua services dimulai" \
  || die "docker compose up gagal"

# Tunggu services healthy
info "Menunggu services sehat (max 60 detik)..."
WAIT=0
while [[ $WAIT -lt 60 ]]; do
  UNHEALTHY=$($COMPOSE_CMD ps --format json 2>/dev/null \
    | grep -c '"Health":"unhealthy"' 2>/dev/null || echo "0")
  STARTING=$($COMPOSE_CMD ps --format json 2>/dev/null \
    | grep -c '"Health":"starting"' 2>/dev/null || echo "0")
  [[ "$STARTING" == "0" && "$UNHEALTHY" == "0" ]] && break
  sleep 5; WAIT=$((WAIT + 5))
  echo -n "."
done
echo ""

# ─────────────────────────────────────────────
# [6] Validasi akhir
# ─────────────────────────────────────────────
header "Status Akhir"

echo ""
$COMPOSE_CMD ps
echo ""

# Cek service kritis
FAILED=0
for svc in routeros router-proxy mosquitto pihole unbound grpc-server; do
  STATUS=$($COMPOSE_CMD ps --format "{{.Service}} {{.State}}" 2>/dev/null \
    | awk -v s="$svc" '$1==s{print $2}' || echo "unknown")
  if [[ "$STATUS" == "running" ]]; then
    ok "$svc: running"
  else
    error "$svc: $STATUS"
    FAILED=$((FAILED + 1))
  fi
done

echo ""
if [[ $FAILED -eq 0 ]]; then
  echo -e "${GREEN}${BOLD}✔ Setup selesai! Semua services berjalan.${NC}"
else
  echo -e "${YELLOW}${BOLD}⚠ Setup selesai dengan $FAILED service bermasalah.${NC}"
  echo "  Cek log: $COMPOSE_CMD logs <service>"
fi

# ─────────────────────────────────────────────
# [7] Ringkasan akses
# ─────────────────────────────────────────────
header "Ringkasan Akses"

CHR_PUBKEY=""
[[ -f /etc/wireguard/publickey ]] && CHR_PUBKEY=$(cat /etc/wireguard/publickey)

echo -e "  ${BOLD}WireGuard${NC}"
echo "    Interface : wg0  (10.10.0.1/24)"
echo "    Port      : 51820/UDP  — buka di firewall VPS"
[[ -n "$CHR_PUBKEY" ]] && echo "    Public Key: $CHR_PUBKEY"
echo ""
echo -e "  ${BOLD}Services (via WireGuard 10.10.0.1)${NC}"
echo "    PiHole Admin  : http://10.10.0.1:8080/admin"
echo "    MQTT Broker   : mqtt://10.10.0.1:1883"
echo "    MQTT WebSocket: ws://10.10.0.1:9001"
echo "    gRPC          : 10.10.0.1:50051"
echo "    MikroTik API  : 10.10.0.1:8728"
echo "    MikroTik SSH  : ssh root@10.10.0.1 -p 2222"
echo ""
echo -e "  ${BOLD}Langkah selanjutnya${NC}"
echo "    1. Buka port 51820/UDP di firewall VPS (UFW: ufw allow 51820/udp)"
echo "    2. Catat WireGuard Public Key CHR di atas"
echo "    3. Jalankan setup di VPS Scraper: sudo bash setup.sh (repo full-tool-browser)"
echo "    4. Tambahkan Public Key VPS Scraper ke /etc/wireguard/wg0.conf [Peer]"
echo "    5. wg addconf wg0 <(wg-quick strip wg0) untuk reload peer tanpa downtime"
echo ""
