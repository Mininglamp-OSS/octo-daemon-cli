VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(DATE)"

.PHONY: build run test clean

build:
	go build $(LDFLAGS) -o bin/octo-daemon ./main.go

run: build
	./bin/octo-daemon start --api-key "$(API_KEY)" --api-url "$(API_URL)"

test:
	go test ./...

clean:
	rm -rf bin/
