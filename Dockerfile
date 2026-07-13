# Cross-compile natively on the build host (pure Go, CGO off) instead of
# emulating the target architecture — keeps multi-arch CI builds fast.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/llm-gateway ./cmd/gateway

# Pure-Go binary (modernc SQLite, no CGO) on a distroless static base,
# running as the non-root user baked into the image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llm-gateway /llm-gateway
# The SQLite database lives here; mount a volume and point
# database.path at /data/gateway.db.
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/llm-gateway"]
CMD ["-config", "/etc/llm-gateway/config.yaml"]
