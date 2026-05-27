FROM golang:1.26.3-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal ./internal
COPY web ./web

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN mkdir -p /out/data \
    && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/redeemer .

FROM scratch

ENV REDEEMER_HOST=0.0.0.0 \
    REDEEMER_PORT=8789 \
    REDEEMER_DB_PATH=/data/redeemer.db

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/redeemer /redeemer

USER 65532:65532

EXPOSE 8789

ENTRYPOINT ["/redeemer"]
CMD ["serve", "--host", "0.0.0.0", "--port", "8789"]
