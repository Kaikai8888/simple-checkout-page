.PHONY: up down reset logs infra run gatesync test

up:      ## build & start everything (app + postgres + valkey + kafka)
	docker compose up --build -d
	@echo "Shop:    http://localhost:8080"
	@echo "Gateway: http://localhost:9090 (mock third-party payment)"

down:
	docker compose down

reset:   ## wipe all data (DB is re-seeded on next start)
	docker compose down -v

logs:
	docker compose logs -f app

infra:   ## only dependencies, then `make run` to run the Go app on the host
	docker compose up -d postgres valkey kafka

run:
	go run ./cmd

gatesync: ## realign flash-sale gates with the DB once (on the host, run it from cron or by hand)
	go run ./cmd/gatesync

test:
	go test ./...
