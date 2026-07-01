# WireGuard: VPS CHR (server side)

Sisi server dari tunnel VPS Scraper (`10.10.0.2`, repo `full-tool-browser`) ↔
VPS CHR (`10.10.0.1`, stack `mikrotik-CHR` — PiHole, Mosquitto, gRPC
orchestrator). Scraper memakai tunnel ini untuk DNS lewat PiHole dan MQTT
lewat Mosquitto, keduanya container Docker di sini.

Sisi client di-setup lewat script terpisah di repo `full-tool-browser`:
`wireguard/setup.sh` — lihat README.md repo itu untuk detail sisi scraper.

## Cara pakai

Butuh docker-compose stack CHR sudah `docker compose up -d` duluan (script
auto-detect network `mikrotik-net` dan IP container `pihole`/`mosquitto`).

```bash
sudo ./setup.sh
```

Script minta **Public Key VPS Scraper** secara interaktif (dari output
`setup.sh` sisi scraper, atau `cat /etc/wireguard/publickey` di VPS Scraper).
Di akhir run, tempel **Public Key VPS CHR** yang ditampilkan ke prompt
`setup.sh` sisi scraper (atau ke `[Peer]` di `wg0.conf` scraper kalau
setup-nya sudah jalan duluan).

Aman dijalankan ulang: `wg0.conf` ditulis ulang total tiap run (bukan
di-append), dan `systemctl restart` di akhir memastikan interface benar-benar
reload konfigurasi baru — penting kalau kamu edit ulang public key peer
scraper atau ganti IP container PiHole/Mosquitto.

## Kalau tidak ada handshake padahal `wg0.conf` sudah benar

Cek dulu apakah **public key di file cocok dengan yang aktif di interface
kernel** — dua hal yang berbeda:

```bash
sudo wg show wg0                      # public key + peer yang AKTIF sekarang
sudo cat /etc/wireguard/wg0.conf | grep PublicKey   # yang ada di FILE
```

Kalau beda, file sudah diedit (mis. peer scraper diganti) tapi interface
belum di-reload — `systemctl restart wg-quick@wg0` (bukan cuma edit file)
untuk apply. Ini gampang kejadian kalau file di-edit manual di luar
`setup.sh`.

## Gotcha: Docker drop forwarded traffic dari wg0 ke bridge-nya sendiri

Symptom: handshake WireGuard sukses, `ping 10.10.0.1` dari scraper jalan,
tapi DNS (`dig @10.10.0.1`) dan MQTT (`10.10.0.1:1883`) tetap gagal.

Root cause (ditemukan via `nft list table ip filter` — `iptables -L` biasa
**tidak bisa dipercaya** di host ini karena backend aktifnya nftables,
counter yang ditampilkan tools legacy basa-basi/stale): Docker generate rule
catch-all di chain filter `DOCKER` miliknya sendiri yang drop semua traffic
forwarded masuk ke bridge Docker dari interface lain (termasuk `wg0`) kecuali
cocok published-port rule. PiHole cuma `expose` port 53 (bukan `ports:`),
jadi DNS-nya selalu kena drop ini walau DNAT di tabel `nat` sudah match.

Untuk MQTT, masalahnya beda: DNAT default di `docker-compose.yml` cuma
scoped ke `daddr 127.0.0.1` (loopback publish), jadi traffic wg0 yang dituju
ke `10.10.0.1:1883` tidak pernah ke-DNAT — paket diterima langsung oleh host
sendiri (karena `10.10.0.1` adalah local address) dan direset karena tidak
ada yang listen di situ.

`setup.sh` di folder ini sudah menulis fix-nya otomatis ke `PostUp`/`PostDown`
`wg0.conf`: bypass drop Docker via `DOCKER-USER` (dievaluasi sebelum
`DOCKER-FORWARD` milik Docker), DNAT khusus wg0 untuk MQTT (terpisah dari
rule `daddr 127.0.0.1` yang sudah ada di `docker-compose.yml` — itu untuk
akses lokal host CHR sendiri, jangan dihapus), dan MASQUERADE
`10.10.0.0/24 -> 172.20.0.0/24` supaya reply dari container ter-translate
balik dengan benar. Harus di `PostUp`/`PostDown`, bukan `iptables -A` manual
— kalau manual dan WireGuard-nya restart/reboot, rule-nya hilang.

Verifikasi: `sudo nft list table ip filter` (bukan `iptables -L`) — cari
counter yang nempel di rule drop chain `DOCKER`, kalau naik tiap kali scraper
coba akses, itu konfirmasi gotcha ini balik lagi (mis. `wg0.conf` ke-reset ke
versi tanpa fix di atas).
