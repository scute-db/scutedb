.PHONY: all test race vet fmt lint bench demo fuzz clean

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
	go run ./cmd/scutedb-demo scan

demo-update:
	go run ./cmd/scutedb-demo update

demo-race:
	go run -race ./cmd/scutedb-demo race

demo-crash:
	go run ./cmd/scutedb-demo crash

demo-pages:
	go run ./cmd/scutedb-demo pages

demo-header:
	go run ./cmd/scutedb-demo header

hexdump: demo-pages
	hexdump -C /tmp/scutedb-pages.db | head -20

clean:
	go clean ./...
	rm -f /tmp/scutedb-pages.db

demo-encode:
	go run ./cmd/scutedb-demo encode

fuzz:
	go test ./internal/codec/ -run=XXX -fuzz=FuzzValueRoundTrip -fuzztime=30s
	go test ./internal/codec/ -run=XXX -fuzz=FuzzKeyOrderingInt64 -fuzztime=30s
	go test ./internal/codec/ -run=XXX -fuzz=FuzzDecoderNeverPanics -fuzztime=30s

demo-nulls:
	go run ./cmd/scutedb-demo nulls

demo-align:
	go run ./cmd/scutedb-demo align

demo-btree:
	go run ./cmd/scutedb-demo btree
