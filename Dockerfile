# Builds the mealie-pantry web companion.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mealie-pantry ./cmd/mealie-pantry

FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 pantry \
 && mkdir /data && chown pantry /data
COPY --from=build /out/mealie-pantry /usr/local/bin/mealie-pantry
USER pantry
ENV DB_PATH=/data/pantry.db LISTEN=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["mealie-pantry"]
