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
ENV LEDGER_ADDR=0.0.0.0:8080
ENTRYPOINT ["pacioli"]
CMD ["serve"]
