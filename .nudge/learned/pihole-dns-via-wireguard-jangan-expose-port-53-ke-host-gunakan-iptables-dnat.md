# PiHole DNS via WireGuard: jangan expose port 53 ke host, gunakan iptables DNAT

## What went wrong
Jika PiHole di-expose dengan `ports: "53:53"` di docker-compose, akan konflik
dengan systemd-resolved yang sudah listen di port 53 host (Ubuntu 22.04+).
Selain itu, binding 0.0.0.0:53 membuka DNS ke seluruh internet.

## Fix
Jangan expose port 53 dari PiHole container ke host. Gunakan iptables DNAT
di PostUp WireGuard untuk forward DNS query dari tunnel ke container:

    # di /etc/wireguard/wg0.conf PostUp:
    iptables -t nat -A PREROUTING -i wg0 -p udp --dport 53 -j DNAT --to-destination 172.20.0.6:53
    iptables -t nat -A PREROUTING -i wg0 -p tcp --dport 53 -j DNAT --to-destination 172.20.0.6:53

172.20.0.6 adalah IP static PiHole container (harus fixed, lihat note subnet).
Query DNS dari VPS Scraper (10.10.0.2) ke 10.10.0.1:53 otomatis di-DNAT ke PiHole.

## Verification
    # dari VPS Scraper setelah WireGuard naik:
    nslookup google.com 10.10.0.1        # harus resolve via PiHole
    nslookup ads.google.com 10.10.0.1   # harus diblokir (return 0.0.0.0)
