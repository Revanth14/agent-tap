.PHONY: build web embed release check clean

VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -s -w -X main.version=$(VERSION)

# build produces the single self-contained ./bin/atap with the UI embedded.
build: embed
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/atap ./cmd/atap

web:
	cd web && npm ci --no-audit --no-fund && npm run build

embed: web
	find internal/server/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R web/dist/. internal/server/dist/

# release builds one tarball per macOS architecture plus checksums.txt in
# bin/release, ready to upload to a GitHub release.
release: embed
	rm -rf bin/release
	for arch in arm64 amd64; do \
		dir=bin/release/atap-$(VERSION)-darwin-$$arch; \
		mkdir -p $$dir && \
		GOOS=darwin GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/atap ./cmd/atap && \
		cp README.md LICENSE $$dir/ && \
		tar -czf $$dir.tar.gz -C bin/release atap-$(VERSION)-darwin-$$arch && \
		rm -rf $$dir || exit 1; \
	done
	cd bin/release && shasum -a 256 *.tar.gz > checksums.txt

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race ./...
	cd web && npm test

clean:
	rm -rf bin web/dist
	find internal/server/dist -mindepth 1 ! -name .gitkeep -delete
