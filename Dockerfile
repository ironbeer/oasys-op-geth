# Build Geth in a stock Go builder container
FROM --platform=$BUILDPLATFORM golang:1.21.3-bullseye as builder

# automatically set by buildkit, can be changed with --platform flag
# Note: This args must not be placed above `FROM`.
#       GOOS and GOARCH will not be able to reference it.
ARG TARGETOS
ARG TARGETARCH

# Support setting various labels on the final image
ARG COMMIT=""
ARG VERSION=""

RUN apt update && apt install -y git

# Get dependencies - will also be cached if we won't change go.mod/go.sum
COPY go.mod /go-ethereum/
COPY go.sum /go-ethereum/
RUN cd /go-ethereum && go mod download

ADD . /go-ethereum
RUN cd /go-ethereum && GOOS=$TARGETOS GOARCH=$TARGETARCH go run build/ci.go install -static ./cmd/geth

# Pull Geth into a second stage deploy debian container
FROM --platform=$TARGETPLATFORM debian:11.9-slim

COPY --from=builder /go-ethereum/build/bin/geth /usr/local/bin/

EXPOSE 8545 8546 30303 30303/udp
ENTRYPOINT ["geth"]

# Add some metadata labels to help programatic image consumption
ARG COMMIT=""
ARG VERSION=""

LABEL commit="$COMMIT" version="$VERSION"
