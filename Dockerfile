# Stage 1: Build the Go application
FROM golang:1.26-alpine AS builder

# Set the working directory
WORKDIR /app

# Copy dependency manifests
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the server binary
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o mtm .

# Build client binaries for cross-platform download
RUN mkdir -p downloads
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags client -ldflags="-w -s" -o downloads/goinstant-windows.exe .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags client -ldflags="-w -s" -o downloads/goinstant-linux .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -tags client -ldflags="-w -s" -o downloads/goinstant-darwin .

# Stage 2: Minimal runtime image
FROM alpine:3.19

# Install ca-certificates (needed for Let's Encrypt / ACME communication)
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy the compiled binary from builder
COPY --from=builder /app/mtm /app/mtm
COPY --from=builder /app/downloads /app/downloads

# Expose ports:
# - 9000 (UDP) for client connections
# - 80 (TCP) for HTTP traffic & Let's Encrypt challenges
# - 443 (TCP) for HTTPS traffic
EXPOSE 9000/udp 80/tcp 443/tcp

# Run the server by default. Override when running client.
ENTRYPOINT ["/app/mtm"]
CMD ["server"]
