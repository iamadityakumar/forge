# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Build the binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/orchestrator ./cmd/orchestrator

# ---- Runtime stage ----
FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/orchestrator /app/orchestrator
EXPOSE 8080
ENTRYPOINT ["/app/orchestrator"]