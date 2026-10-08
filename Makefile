PYTHON ?= python3
VERSION ?= 0.1.0-alpha.1
export GOWORK := off
.PHONY: check test race vet build sdk-test deployment-test compose-check acceptance-test integration fuzz security preview clean

check: test race vet sdk-test deployment-test compose-check acceptance-test integration build

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

build:
	mkdir -p bin
	go build -trimpath -o bin/apostille-local ./cmd/apostille-local
	go build -trimpath -o bin/apostille-local-admin ./cmd/apostille-local-admin
	go build -trimpath -o bin/apostille-local-verify ./cmd/apostille-local-verify

sdk-test:
	PYTHONPATH=sdk/python/src $(PYTHON) -m unittest discover -s sdk/python/tests -v

integration:
	APOSTILLE_TEST_PYTHON="$(abspath $(shell command -v $(PYTHON)))" go test -tags=integration ./tests -count=1 -timeout=60s

deployment-test:
	$(PYTHON) -m unittest discover -s deploy -p 'test_*.py' -v

# Only renders configuration; no Docker daemon, images or GPU are needed.
compose-check:
	$(PYTHON) scripts/validate_compose.py

acceptance-test:
	PYTHONPATH=sdk/python/src $(PYTHON) -m unittest discover -s tools -p 'test_*.py' -v

fuzz:
	go test ./internal/gateway -run='^$$' -fuzz=FuzzStrictJSON -fuzztime=30s

security:
	go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...

# Local preview only; release artifacts still need the actual pinned runtime/model SBOM.
preview:
	mkdir -p dist/$(VERSION)/linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/$(VERSION)/linux-amd64/apostille-local ./cmd/apostille-local
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/$(VERSION)/linux-amd64/apostille-local-admin ./cmd/apostille-local-admin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/$(VERSION)/linux-amd64/apostille-local-verify ./cmd/apostille-local-verify
	$(PYTHON) -m build --outdir dist/$(VERSION)/python sdk/python
	$(PYTHON) -m pip download --require-hashes --no-deps --only-binary=:all: --python-version 3.11 --platform manylinux2014_x86_64 -r sdk/python/requirements-linux-py311.lock --dest dist/$(VERSION)/wheelhouse
	$(PYTHON) tools/preview_manifest.py dist/$(VERSION)

clean:
	rm -rf bin dist
