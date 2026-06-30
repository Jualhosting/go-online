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
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o mtm .

# Stage 2: Minimal runtime image
FROM alpine:3.19

# Install ca-certificates (needed for Let's Encrypt / ACME communication)
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy the compiled binary from builder
COPY --from=builder /app/mtm /app/mtm

# Expose ports:
# - 443 (UDP) for client connections
# - 80 (TCP) for HTTP traffic & Let's Encrypt challenges
# - 443 (TCP) for HTTPS traffic
EXPOSE 443/udp 80/tcp 443/tcp

# Run the server by default. Override when running client.
ENTRYPOINT ["/app/mtm"]
CMD ["server"]
