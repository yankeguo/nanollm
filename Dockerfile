FROM oven/bun:1 AS static
# Mirror the repo layout: main.css resolves its tailwind scan base and
# @source globs relative to web/src/entries/, so web/ must sit at the same
# depth as in the repo — a layout that lets the scan base escape to a
# filesystem root makes tailwind walk the whole container and hang.
WORKDIR /repo/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

FROM golang:1.27 AS builder
ENV CGO_ENABLED=0
WORKDIR /go/src/app
COPY . .
COPY --from=static /repo/web/dist web/dist
RUN go build -trimpath -ldflags="-s -w" -o /nanollm

FROM alpine:3.22
RUN apk add --no-cache tini ca-certificates
COPY --from=builder /nanollm /nanollm
ENV NANOLLM_CONFIG=/config.yaml
EXPOSE 8080
ENTRYPOINT ["tini", "--"]
CMD ["/nanollm"]
