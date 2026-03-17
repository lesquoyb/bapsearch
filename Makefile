COMPOSE = docker compose -f docker/docker-compose.yml
COMPOSE_GPU = docker compose -f docker/docker-compose.yml -f docker/docker-compose.gpu.yml

.PHONY: up down logs build config up-gpu build-gpu config-gpu

up:
	$(COMPOSE) up --build

up-gpu:
	$(COMPOSE_GPU) up --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f

build:
	$(COMPOSE) build

build-gpu:
	$(COMPOSE_GPU) build

config:
	$(COMPOSE) config

config-gpu:
	$(COMPOSE_GPU) config
