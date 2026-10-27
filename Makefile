.PHONY: up down test test-integration check demo
up:
	docker compose up --build -d --wait
down:
	docker compose down
test:
	go test ./...
test-integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'Set TEST_DATABASE_URL to a disposable PostgreSQL database'; exit 1)
	go test -count=1 ./integration/...
check:
	@test -z "$$(gofmt -l cmd internal integration)" || (echo 'Run gofmt'; exit 1)
	go vet ./...
	go test -race -count=1 ./...
demo:
	python3 scripts/demo.py
