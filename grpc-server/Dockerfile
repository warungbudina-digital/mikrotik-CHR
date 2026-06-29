FROM golang:1.22-alpine AS builder

# Install protoc + plugins untuk generate Go code dari .proto
RUN apk add --no-cache protobuf protobuf-dev git
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
RUN go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.4.0

WORKDIR /app

# Download deps dulu (cache layer)
COPY go.mod ./
RUN go mod download || true

# Copy semua source
COPY . .

# Generate pb.go dari .proto
RUN protoc \
    --go_out=. \
    --go_opt=paths=source_relative \
    --go-grpc_out=. \
    --go-grpc_opt=paths=source_relative \
    proto/orchestrator.proto

# Download deps yang mungkin ditambah setelah generate
RUN go mod tidy

# Build binary — static, tidak butuh glibc
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o grpc-server .

# ─────────────────────────────────────────────
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata
ENV TZ=Asia/Jakarta

WORKDIR /app
COPY --from=builder /app/grpc-server .

EXPOSE 9090

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD nc -z localhost 9090 || exit 1

CMD ["./grpc-server"]
