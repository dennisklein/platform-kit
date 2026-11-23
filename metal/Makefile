# Build pk-metal (operator image + CLI). Invoke from the repo root, no cd:
#   make -C metal build test
# Conventions follow control/platform/base/services/docs/assistant/Makefile: podman, the internal
# registry, sha8 tags, never :latest. `publish` (push + roll the Flux-pinned tag) arrives
# with the deployment manifests; CI never pushes anywhere.
IMAGE   ?= registry.pk.internal:5000/pk-metal
TAG     ?= $(shell git rev-parse --short=8 HEAD)
LDFLAGS := -s -w -X main.version=$(TAG)

.PHONY: build test lint lint-phase lint-docs docs image push publish generate manifests envtest parity e2e-kind e2e-qemu

lint:
	golangci-lint run

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/pk-metal ./cmd/pk-metal
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/manager ./cmd/manager

test:
	go test ./...
	go vet -tags e2e ./test/e2e/

# The kind tier: fake the BMC, boot a REAL Talos container, provision through the
# full reconcile path — claim → power-on → maintenance mode → bootstrap → Provisioned.
# Needs docker and the dev cluster (`make dev-up`, context kind-pk-metal).
e2e-kind:
	go test -tags e2e -count 1 -timeout 20m -run TestE2EKind -v ./test/e2e/

# The VM tier: fake the BMC, boot a REAL UEFI VM — config over virtual media, a real
# Talos install to a blank disk, and the reboot that proves it. Downloads the factory
# ISO into .cache/ on first run. Needs qemu-system-x86_64, /dev/kvm, OVMF, and the
# dev cluster (`make dev-up`, context kind-pk-metal).
e2e-qemu:
	go test -tags e2e -count 1 -timeout 40m -run TestE2EQemu -v ./test/e2e/

# The state-model invariant (ADR 0012: conditions are truth, phase/blockedOn are derived
# write-only): reconcilers touch neither — internal/phase.Stamp* is the single write site.
# Greppable by design; fails CI on violation.
lint-phase:
	@! grep -rEn '\.Status\.(Phase|BlockedOn)' internal/controller/ 2>/dev/null \
	  || { echo "lint-phase: reconcilers must not touch .Status.Phase/.Status.BlockedOn — go through internal/phase" >&2; exit 1; }
	@echo "lint-phase: clean"

image: test
	podman build -t $(IMAGE):$(TAG) --build-arg TAG=$(TAG) .

push: image
	podman push $(IMAGE):$(TAG)

publish:
	@echo "publish arrives with the Flux deployment manifests" >&2; exit 1

# controller-gen and setup-envtest are go.mod tool directives — `go tool` builds them
# from the module cache at the pinned version, no separate install step.
generate:
	go tool controller-gen object paths="./api/..."

# reflowdesc unwraps the comment line breaks controller-gen copies verbatim, so
# `kubectl explain` renders flowing text instead of ragged orphan lines.
manifests:
	go tool controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd
	go run ./hack/reflowdesc config/crd/*.yaml

# The field reference in the docs site, generated from the same types the CRDs
# come from. Field prose is generated and behaviour prose is hand-written; the
# two never overlap, so this target only ever rewrites resources.md.
DOCS_DIR ?= ../docs/metal
docs:
	go tool crd-ref-docs --config hack/crd-ref-docs.yaml --source-path ./api \
	  --templates-dir hack/crd-ref-docs-templates \
	  --renderer markdown --output-mode single --output-path $(DOCS_DIR)/.resources.body.md
	@# drop the generator's own H1 (the page title comes from the front matter),
	@# then pin each heading to the anchor the generated links point at
	cat hack/resources-header.md $(DOCS_DIR)/.resources.body.md \
	  | sed '/^# API Reference$$/d' \
	  | awk -f hack/ghanchors.awk > $(DOCS_DIR)/resources.md
	rm -f $(DOCS_DIR)/.resources.body.md

# Every condition reason and phase the API declares must appear in the behaviour
# tables — a reason nobody can act on is a reason that should not exist.
lint-docs:
	@hack/lint-docs.sh

# The render gate (cutover gate later): convert + render must reproduce
# `talhelper genconfig` byte-for-byte for all live nodes, on a throwaway secret.
# Needs the mise-pinned talhelper CLI (same version as the go.mod library pin).
parity:
	hack/parity.sh

# Pinned to the cluster's k8s minor (control/talos/talenv.yaml — same rule as the
# k8s.io/* deps). setup-envtest downloads the control-plane binaries on first run.
ENVTEST_K8S_VERSION ?= 1.36
#
# The suite SKIPS when KUBEBUILDER_ASSETS is unset, and a skip reports `ok` — so an unresolved
# path would take the whole tier green without starting an API server. Resolve it first and
# refuse an empty answer, or a download that failed in CI reads as a pass.
envtest:
	assets=$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path); \
	  [ -n "$$assets" ] || { echo "envtest: no control-plane binaries resolved for k8s $(ENVTEST_K8S_VERSION)" >&2; exit 1; }; \
	  KUBEBUILDER_ASSETS="$$assets" go test ./internal/envtest/
