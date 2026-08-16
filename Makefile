TRAEFIK_VERSION ?= v3.7.10

# Version stamped into the binary. Overridden by CI with the release tag.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMG ?= ghcr.io/alessandrozanatta/gatus-sidecar:$(VERSION)

LDFLAGS := -s -w -X main.version=$(VERSION)
GOTEST_FLAGS ?= -race -count=1

LOCALBIN := $(CURDIR)/bin

# controller-gen is a tool dependency in go.mod, so its version is pinned there,
# its modules land in go.sum where CI's module cache already covers them, and
# Renovate keeps it current along with every other Go dependency.
CONTROLLER_GEN := go tool controller-gen

GO_DIRS := ./cmd ./internal ./api ./test

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: all
all: generate manifests fmt vet build test ## Regenerate, format, build and test.

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

##@ Code generation

.PHONY: generate
generate: ## Generate deepcopy methods.
	$(CONTROLLER_GEN) object:headerFile="" paths=./api/...

.PHONY: manifests
manifests: ## Generate CRD and RBAC manifests.
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=gatus-sidecar paths=./internal/... output:rbac:artifacts:config=deploy/rbac

.PHONY: verify-generated
verify-generated: generate manifests ## Fail if generated files are out of date.
	@git diff --exit-code -- api config/crd/bases deploy/rbac/role.yaml \
		|| { echo "Generated files are stale. Run 'make generate manifests' and commit the result."; exit 1; }

##@ Checks

.PHONY: fmt
fmt: ## Format the source.
	gofmt -w $(GO_DIRS)

.PHONY: fmt-check
fmt-check: ## Fail if the source is not formatted.
	@unformatted="$$(gofmt -l $(GO_DIRS))"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet, including the e2e build tag.
	go vet ./...
	go vet -tags e2e ./test/...

.PHONY: build
build: ## Build the binary.
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/gatus-sidecar ./cmd/gatus-sidecar

.PHONY: test
test: ## Run the unit suite.
	go test ./... $(GOTEST_FLAGS)

.PHONY: test-cover
test-cover: ## Run the unit suite with a coverage summary.
	go test ./... $(GOTEST_FLAGS) -coverprofile=cover.out
	go tool cover -func=cover.out | tail -1

.PHONY: test-e2e
test-e2e: ## Run the end-to-end suite against a kind cluster it creates and deletes.
	go test ./test/e2e/... -tags e2e -count=1 -timeout 20m -v

.PHONY: test-e2e-keep
test-e2e-keep: ## Same, but leave the cluster running for inspection.
	E2E_KEEP_CLUSTER=1 go test ./test/e2e/... -tags e2e -count=1 -timeout 20m -v

.PHONY: e2e-clean
e2e-clean: ## Delete the e2e kind cluster.
	kind delete cluster --name gatus-sidecar-e2e

.PHONY: ci
ci: verify-generated fmt-check vet build test ## Everything CI runs, except the e2e suite.

##@ Packaging

.PHONY: docker-build
docker-build: ## Build the container image.
	docker build --build-arg VERSION=$(VERSION) -t $(IMG) .

.PHONY: install
install: manifests ## Apply the CRD to the current cluster.
	kubectl apply -f config/crd/bases

.PHONY: deploy
deploy: install ## Apply the CRD, RBAC and sample templates.
	kubectl apply -f deploy/rbac
	kubectl apply -f config/samples/endpointtemplates.yaml

##@ Maintenance

.PHONY: update-traefik-crd
update-traefik-crd: ## Re-vendor the Traefik IngressRoute CRD used by the e2e tests.
	curl -sfL https://raw.githubusercontent.com/traefik/traefik/$(TRAEFIK_VERSION)/integration/fixtures/k8s/01-traefik-crd.yml \
		| python3 hack/extract-crd.py ingressroutes.traefik.io $(TRAEFIK_VERSION) \
		> test/e2e/testdata/traefik-ingressroute-crd.yaml

.PHONY: version
version: ## Print the version that would be stamped into a build.
	@echo $(VERSION)
