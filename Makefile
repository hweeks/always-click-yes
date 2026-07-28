# Local development. `make run` is the dogfood loop: build the latest acy from
# this checkout and launch it right here, supervising work on its own repo
# (see AGENTS.md — that is the project's only honest test).

BIN := acy

.PHONY: build run test race live lint fmt clean

build:
	go build -o $(BIN) .

# The 3s countdown is for dogfooding, not a default worth shipping. Children do
# all the tool work now and every one of their calls counts down, so a measured
# run spent ~13 of its 17 minutes waiting on gates it was always going to
# approve. Three seconds is still long enough to hit `s` when you see something
# wrong, which is the only thing the countdown is for.
run: build
	./$(BIN) run --countdown 3s

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
