IMAGE ?= tahrs-scheduler:v0.1.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GO ?= go
GOFMT ?= gofmt

.PHONY: all build test lint vet fmt image manifests rbac replace-sync clean

all: lint test build

build: ## build the scheduler and the replay CLI
	$(GO) build -trimpath -o bin/tahrs-scheduler ./cmd/tahrs-scheduler
	$(GO) build -trimpath -o bin/tahrsctl ./replay

test: ## run the full test suite with the race detector
	$(GO) test -race ./...

vet: ## run go vet
	$(GO) vet ./...

fmt: ## format the tree in place
	$(GOFMT) -w .

lint: vet ## gofmt check + go vet
	@files="$$( $(GOFMT) -l . )"; \
	if [ -n "$$files" ]; then echo "gofmt needs to run on:"; echo "$$files"; exit 1; fi

image: ## build the distroless image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

rbac: ## regenerate deploy/base/clusterrole.yaml from the upstream policies
	$(GO) run ./hack/gen-rbac -output deploy/base/clusterrole.yaml

manifests: ## server-side dry run of deploy/base against the current kubeconfig
	kubectl apply --dry-run=server -k deploy/base

replace-sync: ## regenerate the k8s staging replace block in go.mod
	hack/sync-replace.sh

clean:
	rm -rf bin
