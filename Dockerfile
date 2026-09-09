FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/kasane ./cmd/kasane

FROM alpine:3.22
RUN addgroup -S kasane && adduser -S -G kasane kasane
COPY --from=build /out/kasane /usr/local/bin/kasane
USER kasane
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kasane"]
CMD ["serve"]
