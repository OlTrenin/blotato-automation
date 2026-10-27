FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.22
RUN addgroup -S app && adduser -S -G app app
USER app
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
