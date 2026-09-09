.PHONY: build test vet dev migrate bootstrap reindex compose-up compose-down

build:
	go build ./cmd/kasane
test:
	go test ./...
vet:
	go vet ./...
dev:
	go run ./cmd/kasane serve
migrate:
	go run ./cmd/kasane migrate
bootstrap:
	go run ./cmd/kasane bootstrap --username "$${USERNAME}"
reindex:
	go run ./cmd/kasane reindex $${WORKSPACE:+--workspace "$${WORKSPACE}"}
compose-up:
	docker compose up --build
compose-down:
	docker compose down
