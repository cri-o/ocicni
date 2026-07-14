GO ?= go
PROJECT := github.com/cri-o/ocicni
PREFIX ?= ${DESTDIR}/usr/local
BINDIR ?= ${PREFIX}/bin
LIBEXECDIR ?= ${PREFIX}/libexec
MANDIR ?= ${PREFIX}/share/man
ETCDIR ?= ${DESTDIR}/etc

GIT_COMMIT := $(shell git rev-parse --short HEAD)
BUILD_INFO := $(shell date +%s)

BUILD_PATH := $(shell pwd)/build
GOLANGCI_LINT := ${BUILD_PATH}/golangci-lint
GOLANGCI_LINT_VERSION := v2.11.4

LDFLAGS := -ldflags '-X main.gitCommit=${GIT_COMMIT} -X main.buildInfo=${BUILD_INFO}'

all: binaries

gofmt:
	@./hack/verify-gofmt.sh

binaries: ocicnitool

ocicnitool: $(shell hack/find-godeps.sh $(CURDIR) tools/ocicnitool $(PROJECT))
	$(GO) build $(LDFLAGS) -tags "$(BUILDTAGS)" -o $@ $(PROJECT)/tools/ocicnitool

check:
	@./hack/test-go.sh $(CURDIR)
	@./hack/verify-gofmt.sh
	hack/tree_status.sh

clean:
	rm -rf _output

vendor:
	$(GO) mod tidy && \
	$(GO) mod vendor && \
	$(GO) mod verify

$(GOLANGCI_LINT):
	export VERSION=$(GOLANGCI_LINT_VERSION) \
		URL=https://raw.githubusercontent.com/golangci/golangci-lint \
		BINDIR=${BUILD_PATH} && \
	curl -sSfL $$URL/$$VERSION/install.sh | sh -s $$VERSION

lint:  ${GOLANGCI_LINT}
	${GOLANGCI_LINT} version
	${GOLANGCI_LINT} linters
	GL_DEBUG=gocritic ${GOLANGCI_LINT} run

.PHONY: \
	binaries \
	clean \
	default \
	gofmt \
	help \
	check \
	vendor \
	lint
