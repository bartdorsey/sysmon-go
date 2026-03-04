FROM golang:1.22-alpine AS build
WORKDIR /app
COPY main.go .
COPY static/ static/
RUN go mod init diskmon && go build -o /diskmon .

FROM alpine:3.19
COPY --from=build /diskmon /diskmon
EXPOSE 8080
CMD ["/diskmon"]
