# Module maintenance helpers.

.PHONY: help deps-latest deps-others deps-upgrade tidy test vet

GO ?= go

help:
	@echo "Targets:"
	@echo "  deps-latest   bump direct caerus-framework/* requires to @latest"
	@echo "  deps-others   bump direct non-caerus requires to @latest"
	@echo "  deps-upgrade  go get -u ./..."
	@echo "  tidy          go mod tidy"
	@echo "  test          go test ./..."
	@echo "  vet           go vet ./..."

DIRECT_MODS = awk ' \
	/^require[ \t]+\(/ { inreq=1; next } \
	/^\)/ { inreq=0; next } \
	/^require[ \t]+[^(	 ]/ { \
		if ($$0 !~ /\/\/[ \t]*indirect/) print $$2; \
		next \
	} \
	inreq { \
		if ($$0 ~ /\/\/[ \t]*indirect/) next; \
		print $$1 \
	} \
	' go.mod

deps-latest:
	@set -eu; \
	mods=$$($(DIRECT_MODS) | grep '^github\.com/caerus-framework/' || true); \
	if [ -z "$$mods" ]; then echo "no caerus-framework module deps"; exit 0; fi; \
	args=""; \
	for m in $$mods; do echo "-> $$m@latest"; args="$$args $$m@latest"; done; \
	GOWORK=off $(GO) get $$args; \
	GOWORK=off $(GO) mod tidy; \
	GOWORK=off $(GO) list -m $$mods

deps-others:
	@set -eu; \
	mods=$$($(DIRECT_MODS) | grep -v '^github\.com/caerus-framework/' || true); \
	if [ -z "$$mods" ]; then echo "no non-caerus direct deps"; exit 0; fi; \
	args=""; \
	for m in $$mods; do echo "-> $$m@latest"; args="$$args $$m@latest"; done; \
	GOWORK=off $(GO) get $$args; \
	GOWORK=off $(GO) mod tidy; \
	GOWORK=off $(GO) list -m $$mods

deps-upgrade:
	GOWORK=off $(GO) get -u ./...
	GOWORK=off $(GO) mod tidy

tidy:
	$(GO) mod tidy

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...
