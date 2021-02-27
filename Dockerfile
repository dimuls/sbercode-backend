FROM golang:alpine as builder

RUN apk add --no-cache git

WORKDIR /go/src/github.com/dimuls/sberhack-backend

COPY core ./core    
COPY go.mod go.sum main.go ./

RUN go install .



FROM alpine

ENV GODEBUG="x509ignoreCN=0"

WORKDIR /

COPY --from=builder /go/bin/sberhack-backend ./sberhack-backend
COPY ./ca-bundle.pem ./ca-bundle.pem

ENTRYPOINT [ "/sberhack-backend" ]