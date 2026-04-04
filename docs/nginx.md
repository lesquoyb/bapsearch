# NGINX Reverse Proxy for bapsearch

This guide explains how to set up NGINX as a reverse proxy for the bapsearch backend.

## 1. Configuration File
- Use the provided `docker/nginx.conf` as your NGINX configuration.
- It proxies all traffic to the backend container (default port 8181).

## 2. Usage with Docker Compose
Add an NGINX service to your `docker-compose.yml`:

```yaml
  nginx:
    image: nginx:alpine
    container_name: nginx
    ports:
      - "80:80"
    volumes:
      - ./docker/nginx.conf:/etc/nginx/nginx.conf:ro
    depends_on:
      - backend
```

## 3. Start the stack
```sh
docker compose up -d
```

## 4. Access
- Visit http://localhost/ to access bapsearch via NGINX.

## 5. Customization
- Edit `nginx.conf` to change backend port or add SSL.
