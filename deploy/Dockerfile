# Supply a preloaded, independently verified golang:1.26.9+ image digest.
# The build uses a locally prepared vendor/ tree and --network=none.
ARG GO_BUILD_IMAGE
FROM ${GO_BUILD_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY internal ./internal
COPY cmd/apostille-local ./cmd/apostille-local
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
RUN --network=none go build -mod=vendor -trimpath -ldflags='-s -w' -o /out/apostille-local ./cmd/apostille-local

FROM scratch
COPY --from=build /out/apostille-local /apostille-local
USER 10001:10001
EXPOSE 8443
ENTRYPOINT ["/apostille-local"]
