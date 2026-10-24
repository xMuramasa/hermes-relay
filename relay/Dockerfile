# Build context: relay/  (docker build -t trismegistus-relay relay)
FROM golang:1.27.3-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/trismegistus-relay ./cmd/trismegistus-relay \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trismegistus-relay /usr/local/bin/trismegistus-relay
# Owned by nonroot so a fresh named volume mounted here is writable.
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV DATA_DIR=/data
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/trismegistus-relay", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/trismegistus-relay"]
CMD ["serve"]
