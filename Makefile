APPNAME := pdfmcp
GO ?= go

.PHONY: all test release current clean vet fmt

all: test release current

test:
	CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

fmt:
	$(GO) fmt ./...

release: clean
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o dist/$(APPNAME)-linux-amd64 ./cmd/pdfmcp
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o dist/$(APPNAME)-darwin-arm64 ./cmd/pdfmcp

current:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -o dist/$(APPNAME) ./cmd/pdfmcp

clean:
	mkdir -p dist
	find dist -maxdepth 1 -type f -name '$(APPNAME)*' -delete
