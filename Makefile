.PHONY: all test race vet fmt lint bench demo clean

all: fmt vet test

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

bench:
	go test -bench=. -benchmem ./...

demo:
	@echo "step 0x01 experiments:"
	@echo "  make demo-scan   make demo-update   make demo-race   make demo-crash"
	@echo "step 0x02:"
	@echo "  make demo-pages  make demo-header"

demo-scan:
	go run ./cmd/scute-demo scan

demo-update:
	go run ./cmd/scute-demo update

demo-race:
	go run -race ./cmd/scute-demo race

demo-crash:
	go run ./cmd/scute-demo crash

demo-pages:
	go run ./cmd/scute-demo pages

demo-header:
	go run ./cmd/scute-demo header

hexdump: demo-pages
	hexdump -C /tmp/scute-pages.db | head -20

clean:
	go clean ./...
	rm -f /tmp/scute-pages.db
