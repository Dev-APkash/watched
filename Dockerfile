# syntax=docker/dockerfile:1

# Build on the host's native platform and cross-compile, so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go index.html ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/watched . \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot
LABEL org.opencontainers.image.title="Watched" \
      org.opencontainers.image.description="A tiny self-hosted poster grid of everything you've watched on Jellyfin" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/watched /watched
# Owned by the nonroot user so a fresh named volume mounted here is writable.
COPY --from=build --chown=65532:65532 /out/data /data
USER nonroot:nonroot
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/watched", "-healthcheck"]
ENTRYPOINT ["/watched"]
