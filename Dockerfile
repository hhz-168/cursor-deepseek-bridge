# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/proxy /proxy
USER nobody
ENV LISTEN=:8080
EXPOSE 8080
ENTRYPOINT ["/proxy"]
