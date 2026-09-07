FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/pacioli ./cmd/pacioli

FROM alpine:3.20
RUN adduser -D -u 10001 pacioli
COPY --from=build /out/pacioli /usr/local/bin/pacioli
USER pacioli
EXPOSE 8080

# 0.0.0.0 and not the binary's own default of 127.0.0.1, which is deliberate and
# is not a hole. A published port forwards to the container's network interface,
# so a process bound to the container's loopback cannot be reached through one at
# all — the container would accept nothing. What keeps the port off the network is
# the host side of the publish, and compose.yaml binds that to 127.0.0.1. A
# `docker run -p 8080:8080` without a host address publishes on every interface,
# which is the thing to get right; see "Running it" in README.md.
ENV LEDGER_ADDR=0.0.0.0:8080
ENTRYPOINT ["pacioli"]
CMD ["serve"]
