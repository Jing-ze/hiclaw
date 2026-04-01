ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com

FROM golang:1.25-alpine AS builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY proxy/ ./proxy/
COPY backend/ ./backend/
COPY api/ ./api/
COPY auth/ ./auth/
COPY credentials/ ./credentials/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -o /hiclaw-orchestrator .

FROM ${HIGRESS_REGISTRY}/higress/alpine:3.20
COPY --from=builder /hiclaw-orchestrator /usr/local/bin/
EXPOSE 2375
CMD ["hiclaw-orchestrator"]
