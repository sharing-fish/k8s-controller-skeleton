# Use the official Golang image as the base image
FROM golang:1.23.2 AS builder

# Set the working directory
WORKDIR /app

# Copy go.mod and go.sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod tidy

# Copy the rest of the application code
COPY . .

# Set the entry point for the application
ENTRYPOINT [ "go", "run", "FILE.go" ]