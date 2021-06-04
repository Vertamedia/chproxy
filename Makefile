current_dir = $(pwd)
pkgs = $(shell go list ./...)
gofiles := $(shell find . -name "*.go" -type f -not -path "./vendor/*")

BUILD_TAG = $(or $(shell git tag --points-at HEAD), $(EXT_BUILD_TAG), latest)

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
	@echo "Ver: $(BUILD_TAG), OPTS: $(BUILD_OPTS)"
	@GOOS=linux GOARCH=amd64 go build $(BUILD_OPTS)
	@if [ -f chproxy-linux-amd64-*.tar.gz ]; then rm chproxy-linux-amd64-*.tar.gz; fi
	tar czf chproxy-linux-amd64-$(BUILD_TAG).tar.gz chproxy

release: format lint test clean release-build
	@echo "Ver: $(BUILD_TAG), OPTS: $(BUILD_OPTS)"
	tar czf chproxy-linux-amd64-$(BUILD_TAG).tar.gz chproxy

release-build-docker:
	@echo "Ver: $(BUILD_TAG)"
	@DOCKER_BUILDKIT=1 docker build --target build --build-arg EXT_BUILD_TAG=$(BUILD_TAG) --progress plain -t chproxy-build .
	@docker run --rm --entrypoint "/bin/sh" -v $(CURDIR):/host chproxy-build -c "/bin/cp /go/src/github.com/Vertamedia/chproxy/*.tar.gz /host"
