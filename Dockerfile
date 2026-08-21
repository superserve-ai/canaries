FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/api-canary ./cmd/api-canary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/load-runner ./cmd/load-runner

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api-canary /api-canary
COPY --from=build /out/load-runner /load-runner
ENTRYPOINT ["/api-canary"]
