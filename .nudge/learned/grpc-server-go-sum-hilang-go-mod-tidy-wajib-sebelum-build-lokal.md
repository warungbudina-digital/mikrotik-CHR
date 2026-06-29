# grpc-server go.sum hilang — go mod tidy wajib sebelum build lokal

## What went wrong
grpc-server tidak menyertakan go.sum di repo (file dihasilkan saat Docker
build). Kalau ingin build atau verifikasi kode secara lokal, `go build ./...`
langsung gagal:

  main.go:18:2: missing go.sum entry for module providing package
  github.com/eclipse/paho.mqtt.golang; to add: go get grpc-server

Pesan ini muncul untuk semua dependency sekaligus, bukan hanya yang baru
ditambahkan.

## Fix
Jalankan dari dalam direktori grpc-server:

  cd /home/warungbudina/mikrotik-CHR/grpc-server
  go mod tidy

Ini mengunduh semua dependency dan menghasilkan go.sum. Setelah itu
`go build ./...` berjalan normal.

go.sum yang dihasilkan bisa di-commit agar tidak perlu diulang. Docker
Dockerfile tetap menjalankan `go mod tidy` sendiri di build stage.

## Verification
go build ./...   # harus exit 0 setelah go mod tidy
