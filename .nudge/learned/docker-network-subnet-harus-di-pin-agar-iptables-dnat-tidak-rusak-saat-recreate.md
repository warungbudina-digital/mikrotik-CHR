# Docker network subnet harus di-pin agar iptables DNAT tidak rusak saat recreate

## What went wrong
Jika docker network dibuat tanpa subnet eksplisit, Docker akan auto-assign subnet
yang bisa berubah setiap kali `docker compose down && docker compose up`.
Ini membuat semua iptables DNAT rules (yang hardcode IP container seperti 172.20.0.6)
menjadi invalid — DNS dan gRPC routing akan putus tanpa error yang jelas.

## Fix
Selalu tentukan subnet dan static IP untuk service yang di-referensikan di iptables:

    networks:
      mikrotik-net:
        driver: bridge
        ipam:
          config:
            - subnet: 172.20.0.0/24
              gateway: 172.20.0.1

    services:
      pihole:
        networks:
          mikrotik-net:
            ipv4_address: 172.20.0.6   # harus match dengan iptables DNAT

## Verification
    docker network inspect mikrotik-net | grep -A5 '"Subnet"'
    # harus menampilkan 172.20.0.0/24 konsisten setelah recreate
