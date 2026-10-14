.PHONY: build web check clean

# build produces the single self-contained ./bin/atap with the UI embedded.
build: web
	find internal/server/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R web/dist/. internal/server/dist/
	CGO_ENABLED=0 go build -o bin/atap ./cmd/atap

web:
	cd web && npm ci --no-audit --no-fund && npm run build

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race ./...
	cd web && npm test

clean:
	rm -rf bin web/dist
	find internal/server/dist -mindepth 1 ! -name .gitkeep -delete
