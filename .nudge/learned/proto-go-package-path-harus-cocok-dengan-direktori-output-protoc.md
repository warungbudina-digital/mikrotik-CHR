# Proto go_package path harus cocok dengan direktori output protoc

## What went wrong
grpc-server/main.go mengimport `pb "grpc-server/proto/v1"`, tapi protoc
dengan `--go_opt=paths=source_relative` menaruh file hasil generate di
`proto/orchestrator.pb.go` (bukan `proto/v1/`). go build gagal dengan
pesan menyesatkan:

  main.go:16:2: package grpc-server/proto/v1 is not in std

Pesan "not in std" menyembunyikan bahwa masalahnya adalah path direktori
yang salah, bukan library missing.

## Fix
Sesuaikan option go_package di orchestrator.proto dengan direktori aktual:

```proto
// Sebelum (salah)
option go_package = "grpc-server/proto/v1;orchestratorv1";

// Sesudah (benar — file ada di proto/, bukan proto/v1/)
option go_package = "grpc-server/proto;orchestratorv1";
```

Lalu update import di main.go:
```go
pb "grpc-server/proto"   // bukan proto/v1
```

Regenerate: protoc --go_out=. --go_opt=paths=source_relative \
  --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/orchestrator.proto

## Verification
go build ./...   # harus exit 0 tanpa error "not in std"
