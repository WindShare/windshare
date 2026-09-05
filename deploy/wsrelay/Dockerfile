FROM golang:1.26.5 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY internal ./internal
COPY relay ./relay
RUN CGO_ENABLED=0 go build -trimpath -o /wsrelay ./relay/cmd/wsrelay \
    && mkdir /state && chown 65532:65532 /state

FROM scratch
COPY --from=build /wsrelay /wsrelay
COPY --from=build --chown=65532:65532 /state /state
USER 65532:65532
EXPOSE 8484/tcp 3478/udp
VOLUME ["/state"]
ENTRYPOINT ["/wsrelay"]
CMD ["-state-dir", "/state"]
