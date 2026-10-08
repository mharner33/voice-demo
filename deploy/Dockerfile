# build
FROM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/voicegw ./cmd/voicegw

# run
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 voice
COPY --from=build /out/voicegw /usr/local/bin/voicegw
COPY testdata/ /app/testdata/
USER voice
WORKDIR /app
EXPOSE 5004/udp 50051 8080
ENTRYPOINT ["/usr/local/bin/voicegw"]
