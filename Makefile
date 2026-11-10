GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test e2e vet clean image image-push

# The binary is mounted into machines, so it must be static.
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/cloudsourced ./cmd/cloudsourced

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Control plane container image (Dockerfile). DOCKER="sudo -n docker" if needed.
DOCKER ?= docker
IMAGE ?= cloudsourced-controlplane
image:
	$(DOCKER) build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

# Push to a registry, e.g. the ECR repository of deploy/aws/controlplane.yaml:
#   make image-push IMAGE_REPO=123456789012.dkr.ecr.eu-central-1.amazonaws.com/cloudsourced-controlplane
image-push: image
	@test -n "$(IMAGE_REPO)" || { echo "set IMAGE_REPO"; exit 2; }
	$(DOCKER) tag $(IMAGE):$(VERSION) $(IMAGE_REPO):$(VERSION)
	$(DOCKER) tag $(IMAGE):$(VERSION) $(IMAGE_REPO):latest
	$(DOCKER) push $(IMAGE_REPO):$(VERSION)
	$(DOCKER) push $(IMAGE_REPO):latest

e2e: build
	./test/e2e/run.sh

clean:
	rm -rf bin
