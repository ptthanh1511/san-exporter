# --- STAGE 1: Build the Go application ---
FROM golang:1.22-alpine AS builder

# Set up environment variables for CGO (to link against Alpine's libc)
# and for the build process (to enable reproducible builds).
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

# Set the working directory inside the container to where the Go module lives.
WORKDIR /app

# Copy the go.mod and go.sum files first to cache dependencies.
# Assuming your module name is 'san-exporter' based on the import paths.
COPY go.mod go.sum ./

# Download dependencies (only if go.mod or go.sum changed)
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the application. The output binary is named 'san-exporter'.
RUN go build -ldflags "-s -w" -o /san-exporter ./...

# --- STAGE 2: Create the final lean image ---
FROM alpine:latest

# Use a non-root user for security (optional but recommended)
RUN adduser -D exporter
USER exporter

# Set the working directory
WORKDIR /home/exporter

# Copy the compiled executable from the builder stage
# We only copy the single binary, resulting in a tiny image.
COPY --from=builder /san-exporter /usr/local/bin/san-exporter

# Expose the default port the exporter will run on (e.g., 9090)
EXPOSE 9090

# Command to run the executable when the container starts.
# We pass the port via environment variable (or command line if required by your main.go)
ENTRYPOINT ["/usr/local/bin/san-exporter"]
# CMD ["-port", "9090"]
