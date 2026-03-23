FROM golang:1.25-bookworm AS builder
WORKDIR /app/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/bap-search-backend ./

FROM python:3.12-slim
WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && pip install --no-cache-dir trafilatura \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/bap-search-backend /app/bap-search-backend
COPY ui /app/ui
COPY database /app/database
RUN mkdir -p /models /logs /database
EXPOSE 8081
CMD ["/app/bap-search-backend"]
