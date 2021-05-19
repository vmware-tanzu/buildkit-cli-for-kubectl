# Copyright (C) 2020 VMware, Inc.
# SPDX-License-Identifier: Apache-2.0

VERSION?=$(shell git describe --tags --first-parent --abbrev=7 --long --dirty --always)
TEST_KUBECONFIG?=$(HOME)/.kube/config
TEST_TIMEOUT=600s
TEST_PARALLELISM=3

# Verify Go in PATH
ifeq (, $(shell which go))
$(error You must install Go to build - https://golang.org/dl/ )
endif

INSTALL_DIR=/usr/local/bin
BIN_DIR=./bin
NATIVE_ARCH=$(shell uname | tr A-Z a-z)

GOARCH=amd64
export GOARCH
CI_OSES=linux darwin windows
CI_BUILD_TARGETS=$(foreach os,$(CI_OSES),\
	$(if $(filter windows,$(os)),\
		$(BIN_DIR)/$(os)/kubectl-buildkit.exe $(BIN_DIR)/$(os)/kubectl-build.exe,\
		$(BIN_DIR)/$(os)/kubectl-buildkit $(BIN_DIR)/$(os)/kubectl-build) \
	)
CI_ARCHIVES=$(foreach os,$(CI_OSES),$(BIN_DIR)/$(os).tgz)

GO_MOD_NAME=github.com/vmware-tanzu/buildkit-cli-for-kubectl
GO_DEPS=$(foreach dir,$(shell go list -deps -f '{{.Dir}}' ./cmd/kubectl-buildkit ./cmd/kubectl-build),$(wildcard $(dir)/*.go)) Makefile
REVISION=$(shell git describe --match 'v[0-9]*' --always --dirty --tags)
GO_FLAGS=-ldflags "-X $(GO_MOD_NAME)/version.Version=${VERSION}" -mod=vendor
GO_COVER_FLAGS=-cover -coverpkg=./... -covermode=count
CURRENT_RUNTIME=$(shell kubectl get nodes -o yaml | grep containerRuntimeVersion | grep -v "f:container" | head -1 | cut -f2 -d: | xargs)

.PHONY: help
help:
	@echo "To install the CLI plugin locall run 'make build && sudo make install'"
	@echo "(Note: You must have Go installed on your host to compile)"

.PHONY: clean
clean:
	-rm -rf $(BIN_DIR) cover*.out cover*.html

.PHONY: build
build: $(BIN_DIR)/$(NATIVE_ARCH)/kubectl-buildkit $(BIN_DIR)/$(NATIVE_ARCH)/kubectl-build

$(BIN_DIR)/%/kubectl-buildkit $(BIN_DIR)/%/kubectl-buildkit.exe: $(GO_DEPS)
	GOOS=$* go build $(GO_FLAGS) -o $@ ./cmd/kubectl-buildkit

$(BIN_DIR)/%/kubectl-build $(BIN_DIR)/%/kubectl-build.exe: $(GO_DEPS)
	GOOS=$* go build $(GO_FLAGS) -o $@  ./cmd/kubectl-build

install: $(BIN_DIR)/$(NATIVE_ARCH)/kubectl-buildkit $(BIN_DIR)/$(NATIVE_ARCH)/kubectl-build
	cp $(BIN_DIR)/$(NATIVE_ARCH)/kubectl-buildkit $(BIN_DIR)/$(NATIVE_ARCH)/kubectl-build $(INSTALL_DIR)

print-%:
	@echo '$*=$($*)'

.PHONY: build-ci
build-ci: $(CI_BUILD_TARGETS)

.PHONY: dist
dist: $(CI_BUILD_TARGETS) $(CI_ARCHIVES)

$(BIN_DIR)/%.tgz: $(BIN_DIR)/%/*
	cd $(BIN_DIR)/$* && tar -czvf ../$*.tgz kubectl-*

.PHONY: test
test:
	go test $(GO_FLAGS) $(GO_COVER_FLAGS) -coverprofile=./cover-unit.out ./...

.PHONY: integration
integration:
	@echo "Running integration tests with $(TEST_KUBECONFIG)"
	@kubectl config get-contexts
	TEST_KUBECONFIG=$(TEST_KUBECONFIG) go test -timeout $(TEST_TIMEOUT) $(GO_FLAGS) \
		-parallel $(TEST_PARALLELISM) \
		$(EXTRA_GO_TEST_FLAGS)  \
		$(GO_COVER_FLAGS) -coverprofile=./cover-int-$(CURRENT_RUNTIME).out \
		./integration/...

	go tool cover -html=./cover-int-$(CURRENT_RUNTIME).out -o ./cover-int.html

.PHONY: coverage
coverage: cover.html

cover.html: cover-int-*.out cover-unit.out
	cp cover-unit.out cover.out
	for file in cover-int-*.out; do \
		tail +2 $${file} >> cover.out ; \
	done
	go tool cover -html=./cover.out -o ./cover.html
	go tool cover -func cover.out | grep total:
	open ./cover.html

.PHONY: lint
lint:
	golangci-lint run
