FROM golang:1.22-alpine AS build
WORKDIR /app
COPY main.go .
COPY static/ static/
RUN go mod init sysmon && go build -o /sysmon .

FROM alpine:3.19
COPY --from=build /sysmon /sysmon
EXPOSE 8080
CMD ["/sysmon"]
