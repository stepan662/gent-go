db      ?= gent.db
http    ?= :8080
tcp     ?=
uds     ?=
poll    ?= 500
log     ?= info

# BUILD_FLAGS = CGO_ENABLED=1

.PHONY: run build test test-unit test-int swagger client clean

run:
	$(BUILD_FLAGS) go run ./cmd/gent \
		-db $(db) \
		-http $(http) \
		$(if $(tcp),-tcp $(tcp)) \
		$(if $(uds),-uds $(uds)) \
		-poll $(poll) \
		-log $(log)

build:
	$(BUILD_FLAGS) go build -o gent ./cmd/gent

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

clean:
	rm -f gent $(db)
