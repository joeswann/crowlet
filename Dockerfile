# ---- Builder Stage ----
FROM golang:1.23-alpine AS builder

# Install build dependencies
# Note: git might not be needed if go mod download works without it,
# but keep it if you have private repos or specific version requirements.
RUN apk add --update --no-cache git gcc musl-dev make

# Set up module path (optional, GOPATH is less critical with modules)
# ARG MODULE_PATH=${GOPATH}/src/github.com/Pixep/crowlet
WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build a static, CGO-disabled binary
# Use -ldflags '-extldflags "-static"' for a truly static build
RUN CGO_ENABLED=0 go build \
  -ldflags '-extldflags "-static"' \
  -v -o /crowlet cmd/crowlet/crowlet.go

# ---- Final Stage ----
FROM alpine:latest

# Create a directory for the binary (optional, can place in /usr/local/bin too)
WORKDIR /app

# Copy only the statically compiled binary from the builder stage
COPY --from=builder /crowlet /app/crowlet

# Set the entrypoint
ENTRYPOINT ["/app/crowlet"]

# Optional: Add non-root user for security
# RUN addgroup -S appgroup && adduser -S appuser -G appgroup
# USER appuser
