FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/api-canary ./cmd/api-canary

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api-canary /api-canary
ENTRYPOINT ["/api-canary"]
