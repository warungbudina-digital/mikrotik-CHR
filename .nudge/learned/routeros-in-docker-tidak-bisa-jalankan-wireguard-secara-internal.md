# RouterOS-in-Docker tidak bisa jalankan WireGuard secara internal

## What went wrong
Asumsi awal: WireGuard bisa dikonfigurasi di dalam RouterOS yang berjalan
sebagai Docker container (image dharma007/mikrotik-cloud). Kenyataannya,
RouterOS dalam Docker tidak bisa load kernel module `wireguard` karena
module harus sudah tersedia di host kernel — RouterOS tidak punya mekanisme
`modprobe` ke host. Stability estimate hanya 65-70% vs 95-98% wg-quick di host.

## Fix
Jalankan WireGuard di HOST VPS (wg-quick), bukan di dalam RouterOS container.
RouterOS tetap berjalan sebagai Docker service untuk routing/management:

    apt install wireguard iptables-persistent
    bash wireguard/setup-chr.sh   # generate key + install config
    systemctl enable --now wg-quick@wg0
    netfilter-persistent save     # persist iptables rules

Konfigurasi ada di `wireguard/wg0.conf` di root repo.

## Verification
    wg show                         # tampil interface wg0 + peer
    ping 10.10.0.2                  # ping ke VPS Scraper harus reply
    systemctl status wg-quick@wg0
