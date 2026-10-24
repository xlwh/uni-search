# Dockerfile for UniSearch — uses pre-compiled binary (CGO_ENABLED=0)
# Build locally first: CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o unisearch-linux ./cmd/unisearch
# @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.7
FROM alpine:3.19
RUN apk add --no-cache curl
RUN mkdir /data && chmod 777 /data
COPY unisearch-linux /usr/local/bin/unisearch
RUN chmod +x /usr/local/bin/unisearch
EXPOSE 9200 9300
ENTRYPOINT ["unisearch"]
CMD ["--embedded", "--data-dir", "/data", "--listen", ":9200"]
