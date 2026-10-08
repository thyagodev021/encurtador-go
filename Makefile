.PHONY: build run test race cover bench lint docker clean

build:
	go build -o bin/server ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./...

race:
	go test -race -count=1 ./...

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

bench:
	go test -run xxx -bench . -benchmem ./internal/...

lint:
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

docker:
	docker build -t encurtador-go .

clean:
	rm -rf bin data coverage.out
