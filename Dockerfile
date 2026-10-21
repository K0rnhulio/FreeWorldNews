FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/freeworldnews .

FROM alpine:3
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/freeworldnews ./freeworldnews
COPY config.example.toml ./config.toml
EXPOSE 8080
ENTRYPOINT ["/app/freeworldnews"]
CMD ["-readerOnly", "-readerPort", "8080"]
