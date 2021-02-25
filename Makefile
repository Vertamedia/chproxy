pkgs = $(shell go list ./...)
gofiles := $(shell find . -name "*.go" -type f -not -path "./vendor/*")

BUILD_TAG = $(shell git describe --tags)

BUILD_CONSTS = \
	-X main.buildTime=`date -u '+%Y-%m-%d_%H:%M:%S'` \
	-X main.buildRevision=`git rev-parse HEAD` \
	-X main.buildTag=$(BUILD_TAG)

BUILD_OPTS = -ldflags="$(BUILD_CONSTS)" -gcflags="-trimpath=$(GOPATH)/src"

.PHONY: update format build test run lint reconfigure clean release-build release

format:
	go fmt $(pkgs)
	gofmt -w -s $(gofiles)

build:
	go build

test: build
	go test -race $(pkgs)

run: build
	./chproxy -config=testdata/http.yml

lint:
	go vet $(pkgs)
	go list ./... | grep -v /vendor/ | xargs -n1 golint

reconfigure:
	kill -HUP `pidof chproxy`

clean:
	rm -f chproxy

release-build:
	GOOS=linux GOARCH=amd64 go build $(BUILD_OPTS)

release: format lint test clean release-build
	tar czf chproxy-linux-amd64-$(BUILD_TAG).tar.gz chproxy
