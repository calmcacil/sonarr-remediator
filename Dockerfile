FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /sonarr-remediator ./cmd/sonarr-remediator

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /sonarr-remediator /usr/local/bin/sonarr-remediator
USER 1000:1000
ENTRYPOINT ["sonarr-remediator"]
CMD ["--config", "/config/config.yaml"]
