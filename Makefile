.PHONY: build test run image

build:
	go build -o bin/artifex ./cmd/artifex

test:
	go vet ./...
	go test ./...

# Local server with development sign-in; see README.md, "Run it locally".
run: build
	./bin/artifex serve -config config.yaml

image:
	docker build -t artifex:local .
