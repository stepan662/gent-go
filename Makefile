db      ?= gent.db
http    ?= :8080
tcp     ?=
uds     ?=
poll    ?= 500
log     ?= info

# BUILD_FLAGS = CGO_ENABLED=1

.PHONY: run build test test-unit test-int swagger client clean generate

run:
	$(BUILD_FLAGS) go run ./cmd/gent \
		-db $(db) \
		-http $(http) \
		$(if $(tcp),-tcp $(tcp)) \
		$(if $(uds),-uds $(uds)) \
		-poll $(poll) \
		-log $(log) \
		$(ARGS)

build: sqlc
	$(BUILD_FLAGS) go build -tags "sqlite_omit_load_extension" -ldflags="-s -w" -o gent ./cmd/gent
	$(BUILD_FLAGS) go build -ldflags="-s -w" -o gentctl ./cmd/gentctl

test: test-unit test-int

test-unit:
	$(BUILD_FLAGS) go test ./...

swagger:
	$(BUILD_FLAGS) go run ./cmd/gentspec

schema:
	$(BUILD_FLAGS) go run ./cmd/gentschema $(ARGS)

client: swagger
	cd tests && bun run generate

test-int: client
	cd tests && ~/.bun/bin/bun run typecheck && ~/.bun/bin/bun run test

sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

clean:
	rm -f gent gentctl $(db)
