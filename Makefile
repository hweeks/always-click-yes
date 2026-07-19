# Local development. `make run` is the dogfood loop: build the latest acy from
# this checkout and launch it right here, supervising work on its own repo
# (see AGENTS.md — that is the project's only honest test).

BIN := acy

.PHONY: build run test race live lint fmt clean

build:
	go build -o $(BIN) .

run: build
	./$(BIN) run

test:
	go test ./...

race: ## what CI runs
	go test -race ./...

live: ## real claude, real tokens — needs `claude` on PATH and auth
	ACY_LIVE=1 go test ./... -timeout 30m

lint:
	golangci-lint run ./...

fmt:
	gofmt -l .

clean:
	rm -f $(BIN)
