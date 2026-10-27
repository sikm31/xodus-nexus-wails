BIN     := build/bin/xodus-nexus-wails
VERSION ?= $(shell node -p "require('./frontend/package.json').version")
LDFLAGS := -s -w -X main.Version=$(VERSION)

.PHONY: all build frontend frontend-deps run test test-race clean

all: build

# The Go binary embeds frontend/dist, so the frontend must be built first.
build: frontend
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

frontend: frontend-deps
	cd frontend && npm run build

frontend-deps:
	@[ -d frontend/node_modules ] || (cd frontend && npm ci)

run: build
	./$(BIN)

test: frontend
	go test ./...
	cd frontend && npm test

test-race: frontend
	go test -race ./...

clean:
	rm -rf build/bin frontend/dist
