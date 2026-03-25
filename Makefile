build:
	go build -o ai-menshen ./cmd/ai-menshen

test:
	go test -v ./internal/...

fmt:
	gofmt -w $$(git ls-files '*.go')

fmt-check:
	@if [ -n "$$(git status --porcelain -s)" ]; then \
		echo "Working directory is not clean. Please commit or stash changes before running fmt-check."; \
		exit 1; \
	fi
	@$(MAKE) fmt
	@if [ -z "$$(git status --porcelain)" ]; then \
		echo "All files are formatted correctly."; \
	else \
		echo "The following files were not formatted. Please run 'make fmt' and commit the changes:"; \
		git status --porcelain; \
		exit 1; \
	fi

vet:
	go vet ./...

run:
	go run ./cmd/ai-menshen/main.go -config configs/config.toml

check: fmt-check vet test

.PHONY: build test fmt fmt-check vet check run
