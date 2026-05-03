# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 0.0.2

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# mirror.openshift.io/oc-mirror-bundle:$VERSION and mirror.openshift.io/oc-mirror-catalog:$VERSION.
#IMAGE_TAG_BASE ?= ghcr.io/mariusbertram/oc-mirror-operator
IMAGE_TAG_BASE ?= quay.lab.brtrm.dev/marius
# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)/oc-mirror-operator-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= true
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.42.2
# Image URLs to use for building/pushing image targets
IMG ?= quay.lab.brtrm.dev/marius/oc-mirror-operator:latest
IMG_CONTROLLER ?= quay.lab.brtrm.dev/marius/oc-mirror-operator-controller:latest
IMG_MANAGER ?= quay.lab.brtrm.dev/marius/oc-mirror-operator-manager:latest
IMG_WORKER ?= quay.lab.brtrm.dev/marius/oc-mirror-operator-worker:latest
IMG_DASHBOARD ?= quay.lab.brtrm.dev/marius/oc-mirror-operator-dashboard:latest

# Test/OLM deployment variables
OPERATOR_NAMESPACE ?= oc-mirror-operator
DEFAULT_CHANNEL ?= alpha

# YAML manifests for test deployment (used by deploy-test-catalog)
define CATALOG_SOURCE_YAML
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: oc-mirror-test-catalog
  namespace: $(OPERATOR_NAMESPACE)
spec:
  sourceType: grpc
  image: $(CATALOG_IMG)
  displayName: oc-mirror Test Catalog
  publisher: Local Test
  updateStrategy:
    registryPoll:
      interval: 1m
endef
export CATALOG_SOURCE_YAML

define OPERATOR_GROUP_YAML
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: oc-mirror-operator-group
  namespace: $(OPERATOR_NAMESPACE)
spec: {}
endef
export OPERATOR_GROUP_YAML

define SUBSCRIPTION_YAML
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: oc-mirror-operator
  namespace: $(OPERATOR_NAMESPACE)
spec:
  channel: $(DEFAULT_CHANNEL)
  installPlanApproval: Automatic
  name: oc-mirror
  source: oc-mirror-test-catalog
  sourceNamespace: $(OPERATOR_NAMESPACE)
endef
export SUBSCRIPTION_YAML

# External images used by the operator
IMG_OAUTH_PROXY ?= quay.io/openshift/origin-oauth-proxy:latest
IMG_PROMETHEUS_OPERATOR ?= quay.io/prometheus-operator/prometheus-operator:latest
IMG_PROMETHEUS ?= quay.io/prometheus/prometheus:latest
IMG_GRAFANA ?= docker.io/grafana/grafana:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= podman

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./api/...;./internal/controller/..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/...;./internal/controller/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: hooks
hooks: ## Install git pre-commit hook (runs fmt, vet, lint, tests).
	@ln -sf ../../hack/pre-commit .git/hooks/pre-commit
	@echo "pre-commit hook installed"

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= oc-mirror-test-e2e

# KIND_PROVIDER allows using podman instead of docker as the kind node provider.
# It is auto-detected from CONTAINER_TOOL; override explicitly if needed.
ifeq ($(CONTAINER_TOOL),podman)
KIND_PROVIDER_ENV ?= KIND_EXPERIMENTAL_PROVIDER=podman
else
KIND_PROVIDER_ENV ?=
endif

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND_PROVIDER_ENV) $(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND_PROVIDER_ENV) $(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	$(KIND_PROVIDER_ENV) KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: test-e2e-cluster
test-e2e-cluster: setup-test-e2e docker-build ## Build operator image, load into Kind, and run full cluster e2e tests.
ifeq ($(CONTAINER_TOOL),podman)
	$(CONTAINER_TOOL) save $(IMG) -o /tmp/oc-mirror-e2e.tar
	$(KIND_PROVIDER_ENV) $(KIND) load image-archive /tmp/oc-mirror-e2e.tar --name $(KIND_CLUSTER)
	@rm -f /tmp/oc-mirror-e2e.tar
else
	$(KIND) load docker-image $(IMG) --name $(KIND_CLUSTER)
endif
	KIND_CLUSTER=$(KIND_CLUSTER) CERT_MANAGER_INSTALL_SKIP=true SKIP_CLUSTER_SETUP=true \
		go test ./test/e2e/ -v -ginkgo.v \
		--ginkgo.label-filter="cluster" \
		--ginkgo.timeout=20m
	$(MAKE) cleanup-test-e2e

.PHONY: test-integration
test-integration: fmt vet ## Run integration tests (Cincinnati API + Catalog FBC) without a cluster.
	SKIP_CLUSTER_SETUP=true go test ./test/e2e/ -v -ginkgo.v --ginkgo.label-filter="integration || release || catalog"

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND_PROVIDER_ENV) $(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: build-ui
build-ui: ## Build the React dashboard UI and Console Plugin assets.
	npm --prefix ui ci --ignore-scripts
	npm --prefix ui run build:dashboard
	npm --prefix ui run build:plugin

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## (deprecated) Build docker image with the manager. Use build-images instead.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## (deprecated) Push docker image with the manager. Use push-images instead.
	$(CONTAINER_TOOL) push ${IMG}

# Individual image builds for modular architecture
.PHONY: docker-build-controller
docker-build-controller: ## Build docker image with the controller.
	$(CONTAINER_TOOL) build -t ${IMG_CONTROLLER} -f Dockerfile.controller .

.PHONY: docker-build-manager
docker-build-manager: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG_MANAGER} -f Dockerfile.manager .

.PHONY: docker-build-worker
docker-build-worker: ## Build docker image with the worker (cleanup runs as a subcommand).
	$(CONTAINER_TOOL) build -t ${IMG_WORKER} -f Dockerfile.worker .

.PHONY: docker-build-dashboard
docker-build-dashboard: ## Build docker image for the cluster-wide dashboard (includes React UI build).
	$(CONTAINER_TOOL) build -t ${IMG_DASHBOARD} -f Dockerfile.dashboard .

.PHONY: docker-build-all
docker-build-all: docker-build-controller docker-build-manager docker-build-worker docker-build-dashboard ## (deprecated) Build all modular images. Use build-images instead.

.PHONY: docker-push-controller
docker-push-controller: ## Push controller image.
	$(CONTAINER_TOOL) push ${IMG_CONTROLLER}

.PHONY: docker-push-manager
docker-push-manager: ## Push manager image.
	$(CONTAINER_TOOL) push ${IMG_MANAGER}

.PHONY: docker-push-worker
docker-push-worker: ## Push worker image.
	$(CONTAINER_TOOL) push ${IMG_WORKER}

.PHONY: docker-push-dashboard
docker-push-dashboard: ## Push dashboard image.
	$(CONTAINER_TOOL) push ${IMG_DASHBOARD}

.PHONY: docker-push-all
docker-push-all: docker-push-controller docker-push-manager docker-push-worker docker-push-dashboard ## (deprecated) Push all modular images. Use push-images instead.

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	- $(CONTAINER_TOOL) buildx create --name oc-mirror-builder
	$(CONTAINER_TOOL) buildx use oc-mirror-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile .
	- $(CONTAINER_TOOL) buildx rm oc-mirror-builder

.PHONY: podman-buildx
podman-buildx: ## Build and push multi-arch image for the manager using podman for cross-platform support.
	- podman manifest rm ${IMG}
	podman build --platform $(PLATFORMS) --manifest ${IMG} .
	podman manifest push ${IMG} docker://${IMG}

.PHONY: podman-buildx-local
podman-buildx-local: ## Build multi-arch image for the manager using podman (local, no push).
	- podman manifest rm ${IMG}
	podman build --platform $(PLATFORMS) --manifest ${IMG} .
	@echo "✓ Multi-arch manifest created: ${IMG}"
	@podman manifest inspect ${IMG}

.PHONY: podman-buildx-push
podman-buildx-push: ## Push existing podman manifest to registry.
	podman manifest push ${IMG} docker://${IMG}
	@echo "✓ Manifest pushed to registry: ${IMG}"

.PHONY: podman-buildx-inspect
podman-buildx-inspect: ## Inspect the built multi-arch manifest.
	podman manifest inspect ${IMG}

.PHONY: podman-build-single
podman-build-single: ## (deprecated) Build single-arch image using podman. Use build-images instead.
	podman build -t ${IMG} .
	@echo "✓ Single-arch image built: ${IMG}"

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image \
		controller=${IMG_CONTROLLER} \
		manager=${IMG_MANAGER} \
		worker=${IMG_WORKER} \
		dashboard=${IMG_DASHBOARD} \
		oauth-proxy=${IMG_OAUTH_PROXY}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Images

.PHONY: build-images
build-images: ## Build all operator images (controller, manager, worker, dashboard).
	$(CONTAINER_TOOL) build -t $(IMG_CONTROLLER) -f Dockerfile.controller .
	$(CONTAINER_TOOL) build -t $(IMG_MANAGER) -f Dockerfile.manager .
	$(CONTAINER_TOOL) build -t $(IMG_WORKER) -f Dockerfile.worker .
	$(CONTAINER_TOOL) build -t $(IMG_DASHBOARD) -f Dockerfile.dashboard .

.PHONY: push-images
push-images: ## Push all operator images to registry.
	$(CONTAINER_TOOL) push $(IMG_CONTROLLER)
	$(CONTAINER_TOOL) push $(IMG_MANAGER)
	$(CONTAINER_TOOL) push $(IMG_WORKER)
	$(CONTAINER_TOOL) push $(IMG_DASHBOARD)

.PHONY: build-push-images
build-push-images: build-images push-images ## Build and push all operator images.

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image \
		controller=${IMG_CONTROLLER} \
		manager=${IMG_MANAGER} \
		worker=${IMG_WORKER} \
		dashboard=${IMG_DASHBOARD} \
		oauth-proxy=${IMG_OAUTH_PROXY}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy-test
deploy-test: build-push-images bundle bundle-build catalog-build catalog-push deploy-test-catalog ## Full test deployment: build images, generate bundle, push catalog, deploy via OLM.
	@echo "✅ Operator deployed to test cluster via OLM catalog"

.PHONY: deploy-test-catalog
deploy-test-catalog: ## Apply CatalogSource, OperatorGroup, and Subscription for OLM-based test deployment.
	@echo "Creating namespace $(OPERATOR_NAMESPACE)..."
	-$(KUBECTL) create namespace $(OPERATOR_NAMESPACE) 2>/dev/null || true
	@echo "Applying CatalogSource..."
	@echo "$$CATALOG_SOURCE_YAML" | $(KUBECTL) apply -f -
	@echo "Applying OperatorGroup..."
	@echo "$$OPERATOR_GROUP_YAML" | $(KUBECTL) apply -f -
	@echo "Applying Subscription..."
	@echo "$$SUBSCRIPTION_YAML" | $(KUBECTL) apply -f -
	@echo "Waiting for CSV..."
	-$(KUBECTL) wait --for=jsonpath='{.status.phase}'=Succeeded csv \
		-l operators.coreos.com/oc-mirror-operator.$(OPERATOR_NAMESPACE) \
		-n $(OPERATOR_NAMESPACE) --timeout=300s 2>/dev/null || true
	$(KUBECTL) get csv -n $(OPERATOR_NAMESPACE)

.PHONY: undeploy-test
undeploy-test: ## Remove test catalog deployment from cluster.
	-$(KUBECTL) delete subscription oc-mirror-operator oc-mirror -n $(OPERATOR_NAMESPACE) 2>/dev/null
	-$(KUBECTL) delete operatorgroup --all -n $(OPERATOR_NAMESPACE) 2>/dev/null
	-$(KUBECTL) delete catalogsource oc-mirror-test-catalog -n $(OPERATOR_NAMESPACE) 2>/dev/null
	-$(KUBECTL) delete csv -l operators.coreos.com/oc-mirror.$(OPERATOR_NAMESPACE) -n $(OPERATOR_NAMESPACE) 2>/dev/null
	@echo "✅ Test deployment removed"

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.18.0
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.1.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

.PHONY: operator-sdk
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

##@ Bundle/Catalog

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image \
		controller=$(IMG_CONTROLLER) \
		manager=$(IMG_MANAGER) \
		worker=$(IMG_WORKER) \
		dashboard=$(IMG_DASHBOARD) \
		oauth-proxy=$(IMG_OAUTH_PROXY)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-build
bundle-build: ## Build and push the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .
	$(CONTAINER_TOOL) push $(BUNDLE_IMG)

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMG=$(BUNDLE_IMG)

.PHONY: opm
OPM = $(LOCALBIN)/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.55.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)/oc-mirror-operator-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

# Build a file-based catalog image using opm FBC (File-Based Catalog).
# See: https://olm.operatorframework.io/docs/reference/file-based-catalogs/
.PHONY: catalog-build
catalog-build: opm ## Build a file-based catalog image from the bundle.
	mkdir -p catalog
	# 1. Render bundle objects (CSV, CRDs) from the bundle image
	$(OPM) render $(BUNDLE_IMG) --output yaml > catalog/operator.yaml
	# 2. Append package declaration
	echo '---' >> catalog/operator.yaml
	printf 'schema: olm.package\nname: oc-mirror\ndefaultChannel: %s\n' $(DEFAULT_CHANNEL) >> catalog/operator.yaml
	# 3. Append channel entry linking the bundle into the channel
	echo '---' >> catalog/operator.yaml
	printf 'schema: olm.channel\nname: %s\npackage: oc-mirror\nentries:\n- name: oc-mirror.v%s\n' $(DEFAULT_CHANNEL) $(VERSION) >> catalog/operator.yaml
	# 4. Validate before building
	$(OPM) validate catalog
	# 5. Build catalog image (catalog.Dockerfile already exists in repo)
	$(CONTAINER_TOOL) build -t $(CATALOG_IMG) -f catalog.Dockerfile .

.PHONY: catalog-validate
catalog-validate: opm ## Validate the file-based catalog.
	$(OPM) validate catalog

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) docker-push IMG=$(CATALOG_IMG)
