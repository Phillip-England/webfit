FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /webfit .

FROM alpine:3.22

WORKDIR /app
COPY --from=build /webfit /app/webfit

ENV PORT=8787
EXPOSE 8787

CMD ["sh", "-c", "test -f config/.env || ./webfit init; exec ./webfit -addr 0.0.0.0:8787"]
