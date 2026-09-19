FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/api-canary ./cmd/api-canary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/load-runner ./cmd/load-runner
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/ui-canary ./cmd/ui-canary

FROM mcr.microsoft.com/playwright:v1.57.0-noble AS ui-canary
RUN mkdir -p /root/.cache/ms-playwright-go/1.57.0 && \
    cd /root/.cache/ms-playwright-go/1.57.0 && \
    ln -s /usr/bin/node node && \
    npm pack playwright-core@1.57.0 && \
    tar -xzf playwright-core-1.57.0.tgz && \
    rm playwright-core-1.57.0.tgz
COPY --from=build /out/ui-canary /ui-canary
ENTRYPOINT ["/ui-canary"]

FROM gcr.io/distroless/static-debian12:nonroot AS default
COPY --from=build /out/api-canary /api-canary
COPY --from=build /out/load-runner /load-runner
ENTRYPOINT ["/api-canary"]
