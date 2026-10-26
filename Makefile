.PHONY: up down test logs
up:
	docker compose up --build -d
down:
	docker compose down
test:
	docker compose --profile test run --build --rm integration
logs:
	docker compose logs -f --tail=80
