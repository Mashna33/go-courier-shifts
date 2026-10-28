.PHONY: test test-integration up down smoke fmt

TEST_DB = postgres://shifts:shifts@localhost:5433/shifts_test?sslmode=disable

# Быстрые тесты: интеграционные пропускаются, если не задана TEST_DATABASE_URL.
test:
	go test -race -count=1 ./...

# Полный прогон на настоящей PostgreSQL из docker compose (в отдельной базе shifts_test).
test-integration:
	docker compose up -d --wait postgres
	-docker compose exec -T postgres createdb -U shifts shifts_test
	TEST_DATABASE_URL="$(TEST_DB)" go test -race -count=1 -cover ./...

up:
	docker compose up --build

down:
	docker compose down -v

smoke:
	scripts/smoke.sh

fmt:
	gofmt -w .
