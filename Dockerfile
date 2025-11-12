# --- STAGE 1: Build the Go application ---
# CORRECTED: Updated the base image to Go 1.23 to satisfy the go.mod requirement.
FROM golang:1.23-alpine AS builder

# Set up environment variables for CGO and target architecture.
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64
ENV GOPROXY=https://proxy.golang.org

# Set the working directory inside the container to where the Go module lives.
# This aligns the container's WORKDIR with the module root (src/ in your host system).
WORKDIR /app/src 

# Copy the go.mod and go.sum files first to cache dependencies.
# We copy them from the host's 'src' directory into the container's '/app/src' (the current WORKDIR).
COPY src/go.mod src/go.sum ./

# Download dependencies (only if go.mod or go.sum changed)
RUN go mod download

# Copy the rest of the source code from the host's 'src' directory 
# into the container's '/app/src' (the current WORKDIR).
COPY src/ ./

# Build the application. The output binary is named 'san-exporter'.
# We build the main package (indicated by .) and place the binary in the root for easy access in Stage 2.
RUN go build -ldflags "-s -w" -o /san-exporter .

# --- STAGE 2: Create the final lean image ---
FROM alpine:latest

# Install ca-certificates (needed for most secure network communication from Alpine containers)
RUN apk add --no-cache ca-certificates

# Use a non-root user for security
RUN adduser -D exporter
USER exporter

# Set the working directory (optional for an exporter, but good practice)
WORKDIR /home/exporter

# Copy the compiled executable from the builder stage
# We only copy the single binary, resulting in a tiny image.
COPY --from=builder /san-exporter /usr/local/bin/san-exporter

# FIXED: Changed source path from 'configs/config.yaml' to 'src/configs/config.yaml'
# to correctly locate the file in your host's build context.
COPY src/configs/config.yaml ./configs/config.yaml

# Expose the default port the exporter will run on (e.g., 9090)
EXPOSE 9090

# Command to run the executable when the container starts.
ENTRYPOINT ["/usr/local/bin/san-exporter"]

# You can uncomment the CMD line if your exporter takes the port as a flag.
# CMD ["-port", "9090"]
