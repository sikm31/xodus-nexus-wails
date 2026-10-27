BIN     := build/bin/xodus-nexus-wails
VERSION ?= $(shell node -p "require('./frontend/package.json').version")
LDFLAGS := -s -w -X main.Version=$(VERSION)

.PHONY: all build appimage frontend frontend-deps run test test-race clean

all: build

# The Go binary embeds frontend/dist, so the frontend must be built first.
build: frontend
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

# Self-contained AppImage with GTK and WebKitGTK bundled, for systems where
# they cannot be installed (SteamOS on the Steam Deck has a read-only root).
appimage: build
	go run github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0 generate appimage \
		-binary $(BIN) -icon build/linux/xodus-nexus-wails.png \
		-desktopfile build/linux/xodus-nexus-wails.desktop \
		-builddir build/appimage -outputdir build/bin

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
	rm -rf build/bin build/appimage frontend/dist
