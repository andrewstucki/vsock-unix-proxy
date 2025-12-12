FROM golang:1.25 AS builder

WORKDIR /src
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o /vsock-unix-proxy main.go

FROM scratch
COPY --from=builder /vsock-unix-proxy /vsock-unix-proxy

ENTRYPOINT ["/vsock-unix-proxy"]