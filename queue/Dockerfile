FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/flyc-io/flyc/internal/version.Version=${VERSION}" -o /out/flyc-queue ./queue/cmd/flyc-queue

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 flyc
COPY --from=build /out/flyc-queue /flyc-queue
USER flyc
EXPOSE 8080
ENTRYPOINT ["/flyc-queue"]
