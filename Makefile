# Everything CI does, runnable by hand.

.PHONY: all build test ui ui-test lint clean dev

all: ui build

build:
	go build -o bin/runner-fleet ./cmd/runner-fleet

# The UI is embedded, so building it is part of building the daemon.
ui:
	npm --prefix web ci
	npm --prefix web run build

test:
	go vet ./...
	go test ./...

ui-test:
	npm --prefix web test

lint:
	gofmt -l . | grep -v '^web/' | (! grep .) || (echo "gofmt would change the files above"; exit 1)

# A daemon that touches nothing outside ./tmp, for development.
dev: build
	RUNNER_FLEET_ROOT=$(PWD)/tmp ./bin/runner-fleet serve --addr 127.0.0.1:8080

clean:
	rm -rf bin internal/ui/dist tmp
