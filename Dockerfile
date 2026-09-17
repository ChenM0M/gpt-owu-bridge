FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/gpt-owu-gate ./cmd/gpt-owu-gate \
    && mkdir -p /out/data && chown 65532:65532 /out/data

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/ChenM0M/gpt-owu-bridge"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/gpt-owu-gate /gpt-owu-gate
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV GATE_DATA_DIR=/data
ENTRYPOINT ["/gpt-owu-gate"]
CMD ["serve"]
