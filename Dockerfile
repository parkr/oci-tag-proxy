# Build stage
FROM golang:1.24-alpine AS builder

# Automatically set by buildx based on target platform
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY cmd/ cmd/

# Build the application with ca-certificates embedded
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -installsuffix cgo -o oci-tag-proxy ./cmd/oci-tag-proxy

# Runtime stage - use distroless for smaller image with ca-certificates
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/oci-tag-proxy .

# Expose port
EXPOSE 8080

# Run the application
ENTRYPOINT ["./oci-tag-proxy"]
