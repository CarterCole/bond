# Bond — build, test and package.
#
#   make            build bondd and bond into bin/
#   make test       run the Go test suite
#   make install    install both into /usr/local/bin (sudo)
#   make release    release archives and the Homebrew formula into dist/release
#   make android    build the Android app (debug APK)
#   make ios        generate the Xcode project and build for the simulator
#
# The phone apps embed the Go core, built by `make mobile-android` /
# `make mobile-ios` (which `make android` / `make ios` run first).

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/CarterCole/bond/core/node.Version=$(VERSION)
GOBIN   := $(shell go env GOPATH)/bin
PREFIX  ?= /usr/local

# A release is named for its tag without the "v". RELEASE_URL is where its
# archives can be downloaded from, which the Homebrew formula points at.
RELEASE_VERSION = $(patsubst v%,%,$(VERSION))
RELEASE_URL ?= https://github.com/CarterCole/bond/releases/download/v$(RELEASE_VERSION)

# Android SDK: wherever it is, as long as it has an NDK (r28+ for 16 KB pages).
ANDROID_HOME ?= $(firstword $(wildcard $(HOME)/Library/Android/sdk /opt/homebrew/share/android-commandlinetools $(HOME)/Android/Sdk))
ANDROID_NDK_HOME ?= $(lastword $(sort $(wildcard $(ANDROID_HOME)/ndk/*)))
export ANDROID_HOME ANDROID_NDK_HOME

.PHONY: all build test test-short race vet fmt install clean dist release \
        gomobile mobile-android mobile-ios android ios

all: build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test:
	go test ./...

test-short:
	go test -short ./...

race:
	go test -race -short ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l cmd core web)" || (echo "gofmt needed:"; gofmt -l cmd core web; exit 1)

fmt:
	gofmt -w cmd core web

install: build
	install -m 0755 bin/bondd bin/bond $(PREFIX)/bin/

clean:
	rm -rf bin dist android/app/libs/bond.aar android/app/libs/bond-sources.jar ios/Frameworks/Bond.xcframework

# Static binaries for servers and routers.
dist:
	@mkdir -p dist
	@for t in linux/amd64 linux/arm64 linux/arm darwin/arm64 darwin/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/bond-$$os-$$arch/ ./cmd/... || exit 1; \
	done

# One archive per platform, their checksums, and the Homebrew formula that
# installs them (dist/release/bond.rb, for the tap). Each archive carries the
# license and NOTICE, the licenses of the third-party code compiled in.
RELEASE_TARGETS := linux/amd64 linux/arm64 linux/arm darwin/arm64 darwin/amd64

release:
	@rm -rf dist/release && mkdir -p dist/release
	@for t in $(RELEASE_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/release/bond-$(RELEASE_VERSION)-$$os-$$arch/ ./cmd/... || exit 1; \
	done
	@deploy/notices.sh dist/release/*/bondd dist/release/*/bond > dist/release/NOTICE
	@for d in dist/release/bond-*/; do \
		name=$$(basename $$d); echo "  $$name.tar.gz"; \
		cp README.md LICENSE dist/release/NOTICE $$d; \
		COPYFILE_DISABLE=1 tar -C dist/release -czf dist/release/$$name.tar.gz $$name || exit 1; \
		rm -r $$d; \
	done
	@rm dist/release/NOTICE
	@cd dist/release && shasum -a 256 *.tar.gz > checksums.txt
	@deploy/homebrew/formula.sh $(RELEASE_VERSION) $(RELEASE_URL) dist/release > dist/release/bond.rb
	@echo "  bond.rb"

# --- phones -----------------------------------------------------------------

gomobile:
	@test -x $(GOBIN)/gomobile || go install golang.org/x/mobile/cmd/gomobile@latest
	@test -x $(GOBIN)/gobind   || go install golang.org/x/mobile/cmd/gobind@latest

# The core as an Android library. -androidapi 26: the NDK no longer supports
# gomobile's default of 16.
mobile-android: gomobile
	@test -n "$(ANDROID_NDK_HOME)" || (echo "No Android NDK found. Install one: sdkmanager 'ndk;28.2.13676358'"; exit 1)
	@mkdir -p android/app/libs
	PATH="$(GOBIN):$$PATH" gomobile bind -target android -androidapi 26 -trimpath \
		-ldflags '$(LDFLAGS)' -o android/app/libs/bond.aar ./core/mobile

# The core as an Apple framework for devices and simulators.
mobile-ios: gomobile
	@mkdir -p ios/Frameworks
	PATH="$(GOBIN):$$PATH" gomobile bind -target ios,iossimulator -iosversion 17.0 -trimpath \
		-ldflags '$(LDFLAGS)' -o ios/Frameworks/Bond.xcframework ./core/mobile

android: mobile-android
	cd android && ./gradlew --console=plain assembleDebug

ios: mobile-ios
	cd ios && xcodegen generate
	cd ios && xcodebuild -project Bond.xcodeproj -scheme Bond -sdk iphonesimulator \
		-destination 'generic/platform=iOS Simulator' -configuration Debug \
		CODE_SIGNING_ALLOWED=NO build
