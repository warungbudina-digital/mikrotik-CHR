#!/bin/bash
set -euo pipefail

# =============================================================================
# Generate Mosquitto password file
# Jalankan sekali sebelum docker compose up
# =============================================================================

PASSWD_FILE="$(dirname "$0")/passwd"

if [[ -f "$PASSWD_FILE" ]]; then
  echo "Password file sudah ada: $PASSWD_FILE"
  echo "Hapus file tersebut jika ingin buat ulang."
  exit 0
fi

echo "=== Buat MQTT credentials ==="
echo ""

read -rp "MQTT Username (default: browser-agent): " MQTT_USER
MQTT_USER="${MQTT_USER:-browser-agent}"

read -rsp "MQTT Password: " MQTT_PASS
echo ""
read -rsp "Konfirmasi Password: " MQTT_PASS2
echo ""

if [[ "$MQTT_PASS" != "$MQTT_PASS2" ]]; then
  echo "Password tidak cocok. Coba lagi."
  exit 1
fi

# Generate password file menggunakan Docker (tidak perlu install mosquitto di host)
docker run --rm \
  -v "$(dirname "$0"):/passwd_dir" \
  eclipse-mosquitto:2 \
  sh -c "mosquitto_passwd -b -c /passwd_dir/passwd '$MQTT_USER' '$MQTT_PASS'"

echo ""
echo "Password file dibuat: $PASSWD_FILE"
echo "Username: $MQTT_USER"
echo ""
echo "Simpan credentials ini di .env:"
echo "  MQTT_USERNAME=$MQTT_USER"
echo "  MQTT_PASSWORD=<password yang baru diinput>"
