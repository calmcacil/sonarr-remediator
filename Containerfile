FROM docker.io/library/golang:1.27.3-alpine3.23@sha256:8a5046497cda39860b832ffdbe8dd66018a0f05bb01352f533a36e7e2ced66a0 AS build
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY cmd/sonarr-remediator ./cmd/sonarr-remediator
COPY VERSION ./VERSION
RUN go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/sonarr-remediator ./cmd/sonarr-remediator && mkdir -p /out/state && chmod 0700 /out/state

FROM scratch
ARG VERSION=0.5.0
ARG REVISION=unknown
LABEL org.opencontainers.image.title="sonarr-remediator" org.opencontainers.image.version=$VERSION org.opencontainers.image.revision=$REVISION org.opencontainers.image.source="https://github.com/calmcacil/sonarr-remediator"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/sonarr-remediator /sonarr-remediator
COPY --from=build --chown=65532:65532 /out/state /state
USER 65532:65532
ENTRYPOINT ["/sonarr-remediator"]
