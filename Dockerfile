FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
COPY static/ static/
RUN CGO_ENABLED=0 go build -o /sysmon .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends systemd && rm -rf /var/lib/apt/lists/*
COPY --from=build /sysmon /sysmon
EXPOSE 8080
CMD ["/sysmon"]
