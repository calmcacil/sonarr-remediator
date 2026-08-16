FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS TARGETARCH
ARG VERSION=dev

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags="-s -w -X main.version=${VERSION}" -o /sonarr-remediator ./cmd/sonarr-remediator

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /sonarr-remediator /sonarr-remediator

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/sonarr-remediator", "--healthcheck", "--config", "/config/config.yaml"]

ENTRYPOINT ["/sonarr-remediator"]
CMD ["--config", "/config/config.yaml"]
