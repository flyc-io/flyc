FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/flyc-io/flyc/internal/version.Version=${VERSION}" -o /out/flyc-control ./control/cmd/flyc-control

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 flyc && mkdir -p /data && chown flyc /data
COPY --from=build /out/flyc-control /flyc-control
USER flyc
VOLUME /data
EXPOSE 8090
ENTRYPOINT ["/flyc-control"]
