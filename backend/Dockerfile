FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN go build -o /studio ./cmd/studio
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 studio
COPY --from=build /studio /usr/local/bin/studio
USER studio
ENTRYPOINT ["studio"]
