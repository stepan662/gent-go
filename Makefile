db      ?= gent.db
http    ?= :8080
tcp     ?=
uds     ?=
poll    ?= 500
log     ?= info

BUILD_FLAGS = CGO_ENABLED=1

.PHONY: run build clean

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

clean:
	rm -f gent $(db)
