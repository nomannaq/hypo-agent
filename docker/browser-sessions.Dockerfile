FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/browser-sessions ./cmd/browser-sessions
COPY internal/dotenv ./internal/dotenv
COPY internal/model ./internal/model
COPY internal/tool ./internal/tool
RUN CGO_ENABLED=0 go build -trimpath -o /browser-sessions ./cmd/browser-sessions

FROM alpine:3.23
COPY --from=build /browser-sessions /usr/local/bin/browser-sessions
USER 65532:65532
ENTRYPOINT ["browser-sessions"]
