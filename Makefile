SHELL := /usr/bin/env bash

GOFLAGS ?=
KO_DOCKER_REPO ?= ko.local
KARPENTER_NAMESPACE ?= karpenter
CLUSTER_NAME ?=
STACKIT_PROJECT_ID ?=
STACKIT_REGION ?=

# Karpenter core owns the NodePool, NodeClaim and NodeOverlay CRDs. They are vendored into
# pkg/apis/crds so the controller installs exactly the versions it was built against, rather than
# whatever happens to be in the cluster.
#
# The module's on-disk location is resolved inside the recipe, not here: `go list -m -f '{{.Dir}}'`
# returns an empty string until the module has been extracted into the module cache, so evaluating
# it at parse time silently yields `cp /pkg/apis/...` on a cold cache such as a fresh CI runner.
KARPENTER_CORE_CRDS := karpenter.sh_nodepools karpenter.sh_nodeclaims karpenter.sh_nodeoverlays

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile the controller
	go build $(GOFLAGS) ./...

.PHONY: test
test: ## Run unit tests
	go test $(GOFLAGS) -race ./...

.PHONY: coverage
coverage: ## Run unit tests and write a coverage profile
	go test $(GOFLAGS) -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	go tool -modfile=go.tools.mod golangci-lint run

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: generate
generate: codegen vendor-crds ## Regenerate deepcopy funcs and CRDs

.PHONY: codegen
codegen: ## Regenerate deepcopy functions and the StackitNodeClass CRD
	cd pkg/apis && go tool -modfile=../../go.tools.mod controller-gen \
		crd object:headerFile="../../hack/boilerplate.go.txt" \
		paths="./..." output:crd:artifacts:config=crds

.PHONY: vendor-crds
vendor-crds: ## Copy the Karpenter core CRDs matching the pinned karpenter version
	@set -euo pipefail; \
	go mod download sigs.k8s.io/karpenter; \
	version=$$(go list -m -f '{{.Version}}' sigs.k8s.io/karpenter); \
	dir=$$(go list -m -f '{{.Dir}}' sigs.k8s.io/karpenter); \
	if [ -z "$$dir" ]; then \
		echo "could not resolve sigs.k8s.io/karpenter in the module cache" >&2; \
		exit 1; \
	fi; \
	echo "vendoring karpenter core CRDs from $$version"; \
	for crd in $(KARPENTER_CORE_CRDS); do \
		cp "$$dir/pkg/apis/crds/$$crd.yaml" pkg/apis/crds/; \
	done; \
	chmod +w pkg/apis/crds/*.yaml; \
	cp pkg/apis/crds/*.yaml charts/karpenter-crd/templates/

.PHONY: verify
verify: generate tidy ## Regenerate everything and fail if anything changed
	@git diff --exit-code || (echo "generated files are out of date, run 'make generate tidy'" && exit 1)

.PHONY: image
image: ## Build and publish the controller image with ko
	# --bare uses KO_DOCKER_REPO verbatim instead of appending the Go import path, keeping the
	# result aligned with image.repository in the chart's values.yaml.
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) go tool -modfile=go.tools.mod ko build --bare ./cmd/controller

.PHONY: package
package: ## Package both charts into dist/ at VERSION (defaults to the chart's own version)
	mkdir -p dist
	helm package charts/karpenter-crd --destination dist $(if $(VERSION),--version $(VERSION) --app-version $(VERSION))
	helm package charts/karpenter --destination dist $(if $(VERSION),--version $(VERSION) --app-version $(VERSION))

.PHONY: install
install: ## Install the chart into the current cluster context
	@test -n "$(CLUSTER_NAME)" || (echo "CLUSTER_NAME is required" && exit 1)
	@test -n "$(STACKIT_PROJECT_ID)" || (echo "STACKIT_PROJECT_ID is required" && exit 1)
	@test -n "$(STACKIT_REGION)" || (echo "STACKIT_REGION is required" && exit 1)
	helm upgrade --install karpenter charts/karpenter \
		--namespace $(KARPENTER_NAMESPACE) --create-namespace \
		--set settings.clusterName=$(CLUSTER_NAME) \
		--set settings.projectID=$(STACKIT_PROJECT_ID) \
		--set settings.region=$(STACKIT_REGION) \
		--wait

.PHONY: uninstall
uninstall: ## Remove the chart from the current cluster context
	helm uninstall karpenter --namespace $(KARPENTER_NAMESPACE)

.PHONY: all
all: build test vet lint ## Build, test and lint
