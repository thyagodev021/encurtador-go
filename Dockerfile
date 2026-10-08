# --- build ---
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- imagem final, pequena e sem root ---
FROM alpine:3.20
RUN adduser -D -u 10001 app && mkdir /data && chown app /data
USER app
COPY --from=build /out/server /server
VOLUME /data
EXPOSE 8080
ENV DATA_FILE=/data/links.jsonl
ENTRYPOINT ["/server"]
