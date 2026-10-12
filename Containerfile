FROM docker.io/library/golang:1.27.2-alpine3.23@sha256:9e45f0eb4a63ed37ad6e604950407558b7c738e34e015ae77a5d4ad369cde079 AS build
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY cmd/sonarr-remediator ./cmd/sonarr-remediator
COPY VERSION ./VERSION
RUN go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/sonarr-remediator ./cmd/sonarr-remediator && mkdir -p /out/state && chmod 0700 /out/state

FROM scratch
ARG VERSION=0.3.0
ARG REVISION=unknown
LABEL org.opencontainers.image.title="sonarr-remediator" org.opencontainers.image.version=$VERSION org.opencontainers.image.revision=$REVISION org.opencontainers.image.source="https://github.com/calmcacil/sonarr-remediator"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/sonarr-remediator /sonarr-remediator
COPY --from=build --chown=65532:65532 /out/state /state
USER 65532:65532
ENTRYPOINT ["/sonarr-remediator"]
