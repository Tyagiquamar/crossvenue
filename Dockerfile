FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/crossvenue ./cmd/crossvenue \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/replay ./cmd/replay \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loadgen ./cmd/loadgen

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY --from=build /src/migrations/ /migrations/
COPY config.example.yaml /etc/crossvenue/config.yaml
EXPOSE 8471
ENTRYPOINT ["crossvenue", "--config", "/etc/crossvenue/config.yaml"]
