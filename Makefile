# Simple Makefile for go_crawler
# Usage examples:
#   make build
#   make run CONFIG=config.example.yaml
#   make fmt
#   make tidy
#   make clean

GO ?= go
BIN ?= bin/go-crawler
PKG ?= ./cmd/go-crawler
CONFIG ?= config.yaml

.PHONY: all build run fmt tidy clean

all: build

build: fmt tidy
	$(GO) build -o $(BIN) $(PKG)

run: build
	$(BIN) -config $(CONFIG)

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin build dist