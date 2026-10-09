FROM golang:1.26.5-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/bob ./cmd/bob

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/bob /app/bob
COPY config.yaml.example /app/config.yaml

EXPOSE 8080

ENTRYPOINT ["/app/bob", "-config", "/app/config.yaml"]
