FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /eventic-server ./server/cmd/

FROM gcr.io/distroless/static-debian12
COPY --from=build /eventic-server /eventic-server
USER nonroot:nonroot
ENTRYPOINT ["/eventic-server"]
