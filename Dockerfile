# Note: Do not place `ARG` above `FROM`.
#       It will not be able to be referenced by RUN.

# Build Geth in a stock Go builder container
FROM --platform=$BUILDPLATFORM golang:1.21-alpine as builder

# Support setting various labels on the final image
ARG COMMIT=""
ARG VERSION=""
ARG BUILDNUM=""

# automatically set by buildkit, can be changed with --platform flag
ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache gcc musl-dev linux-headers git

# Get dependencies - will also be cached if we won't change go.mod/go.sum
COPY go.mod /go-ethereum/
COPY go.sum /go-ethereum/
RUN cd /go-ethereum && go mod download

ADD . /go-ethereum
RUN cd /go-ethereum && \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go run build/ci.go install -static ./cmd/geth

# Pull Geth into a second stage deploy alpine container
FROM --platform=$TARGETPLATFORM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder /go-ethereum/build/bin/geth /usr/local/bin/

EXPOSE 8545 8546 30303 30303/udp
ENTRYPOINT ["geth"]

# Add some metadata labels to help programatic image consumption
ARG COMMIT=""
ARG VERSION=""
ARG BUILDNUM=""

LABEL commit="$COMMIT" version="$VERSION" buildnum="$BUILDNUM"
