FROM golang:1.19-alpine3.16 AS builder
WORKDIR /app
COPY ./ ./
RUN CGO_ENABLED=0 go build -o /bin/app main.go

FROM scratch
COPY --from=builder /bin/app /bin/app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["app"]