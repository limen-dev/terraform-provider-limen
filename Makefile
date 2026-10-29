# Common tasks. Tools come from mise.toml: run `mise install` first.
SHELL := /bin/bash
TOFU := $(shell command -v tofu)

.PHONY: build install test testacc testacc-tofu lint generate docs proto fake check

build:
	go build ./...

# Builds into ./bin. Point dev_overrides at it (see CONTRIBUTING.md).
install:
	go build -o bin/terraform-provider-limen .

test:
	go test ./...

# Acceptance tests drive real terraform and tofu binaries against the
# in-memory Limen (internal/fakelimen); they need no control plane.
testacc:
	TF_ACC=1 go test ./internal/provider/ -count=1 -v

testacc-tofu:
	TF_ACC=1 TF_ACC_TERRAFORM_PATH=$(TOFU) TF_ACC_PROVIDER_HOST=registry.opentofu.org go test ./internal/provider/ -count=1 -v

lint:
	golangci-lint run ./...
	buf lint proto
	actionlint

generate: proto docs

proto:
	buf generate

docs:
	go tool tfplugindocs generate --provider-name limen
	terraform fmt -recursive examples

# Runs the in-memory Limen for trying the provider by hand.
fake:
	go run ./tools/fakelimen -dir .fakelimen

check: lint test testacc testacc-tofu
