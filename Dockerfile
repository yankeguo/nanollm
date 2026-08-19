FROM golang:1.26 AS builder
ENV CGO_ENABLED=0
WORKDIR /go/src/app
ADD . .
RUN go build -o /nanollm

FROM alpine:3.22
RUN apk add --no-cache tini ca-certificates
COPY --from=builder /nanollm /nanollm
ENV NANOLLM_CONFIG=/config.yaml
ENTRYPOINT ["tini", "--"]
CMD ["/nanollm"]
