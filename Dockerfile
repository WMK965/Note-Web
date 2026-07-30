FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o note-web -ldflags="-s -w" .

FROM alpine:3.20
RUN apk add --no-cache poppler-utils mupdf-tools
COPY --from=builder /app/note-web /usr/local/bin/
EXPOSE 8080
VOLUME ["/data"]
CMD ["note-web", "-config", "/data/config.yaml"]
