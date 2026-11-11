GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test e2e vet clean image image-push

# The binary is mounted into machines, so it must be static. Machines of
# the other architecture (aws-ec2 sizes with arch = "arm64" or "amd64") run
# bin/offshore-linux-<arch>, which the provider finds next to the
# orchestrator's binary.
HOST_PLATFORM := $(shell $(GO) env GOOS)/$(shell $(GO) env GOARCH)
MACHINE_ARCHES ?= amd64 arm64
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/offshore ./cmd/offshore
	@for a in $(MACHINE_ARCHES); do \
	  if [ "$(HOST_PLATFORM)" != "linux/$$a" ]; then \
	    echo "building bin/offshore-linux-$$a"; \
	    CGO_ENABLED=0 GOOS=linux GOARCH=$$a $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/offshore-linux-$$a ./cmd/offshore || exit 1; \
	  fi; \
	done

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Control plane container image (Dockerfile). DOCKER="sudo -n docker" if needed.
DOCKER ?= docker
IMAGE ?= offshore-controlplane
image:
	$(DOCKER) build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

# Push to a registry, e.g. the ECR repository of deploy/aws/controlplane.yaml:
#   make image-push IMAGE_REPO=123456789012.dkr.ecr.eu-central-1.amazonaws.com/offshore-controlplane
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
