# Stage 1: Build the Go application
FROM golang:1.19-alpine AS builder

# Set the target architecture
ARG TARGETARCH
ENV GOARCH=arm64

# Set the working directory inside the container
WORKDIR /app

# Copy the Go modules files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code into the container
COPY . .

# Build the Go application for the specified architecture
RUN GOOS=linux GOARCH=$GOARCH go build -o myapp

# Stage 2: Create a small image with the compiled Go binary
FROM alpine:latest

# Set the working directory in the new container
WORKDIR /app

# Copy the compiled Go binary from the builder stage
COPY --from=builder /app/myapp .

# Add execute permission for the binary
RUN chmod +x myapp

# Expose the port the app runs on
EXPOSE 8080

# Command to run the application
CMD ["./myapp"]