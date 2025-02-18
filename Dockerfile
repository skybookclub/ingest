# syntax=docker/dockerfile:1

FROM golang:latest AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/ingest

FROM alpine:latest

COPY --from=builder /app/ingest /app/ingest

CMD [ "/app/ingest" ]
