FROM rust:1-alpine AS build
RUN apk add --no-cache musl-dev gcc
WORKDIR /src
COPY relay-rust ./relay-rust
RUN cargo build --manifest-path relay-rust/Cargo.toml --locked --release \
    --package windshare-relay-native --bin relay-native --target-dir /build \
    && mkdir /state && chown 65532:65532 /state

FROM scratch
COPY --from=build /build/release/relay-native /wsrelay
COPY --from=build --chown=65532:65532 /state /state
USER 65532:65532
EXPOSE 8484/tcp 3478/udp
VOLUME ["/state"]
ENTRYPOINT ["/wsrelay"]
CMD ["--database", "/state/relay.sqlite", "--listen", "0.0.0.0:8484"]
