# `make help`
.PHONY: help
help:
	@cat Makefile | grep '# `' | grep -v '@cat Makefile'

# `make build`
.PHONY: build
build:
	CGO_ENABLED=1 go build -o ./build/sys-task-manager -mod vendor -trimpath -ldflags '-s -w' .

# `make build-prepare-on-debian-amd64`
.PHONY: build-prepare-on-debian-amd64
build-prepare-on-debian-amd64:
	sudo apt update -qq
	sudo apt install -qq -y gcc gcc-aarch64-linux-gnu

# `make build-all-platform-linux`
.PHONY: build-all-platform-linux
build-all-platform-linux:
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
		go build -o ./build/sys-task-manager_linux_amd64  -mod vendor -trimpath -ldflags '-s -w' .
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
		go build -o ./build/sys-task-manager_linux_arm64  -mod vendor -trimpath -ldflags '-s -w' .

# `make build-all-platform-linux-via-docker`
.PHONY: build-all-platform-linux-via-docker
build-all-platform-linux-via-docker:
	docker run --rm -v "$$(pwd):/src" --workdir /src debian:10 bash build-in-docker.sh

# `make build-all-platform-darwin`
.PHONY: build-all-platform-darwin
build-all-platform-darwin:
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 \
		go build -o ./build/sys-task-manager_darwin_amd64  -mod vendor -trimpath -ldflags '-s -w' .
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
		go build -o ./build/sys-task-manager_darwin_arm64  -mod vendor -trimpath -ldflags '-s -w' .

# `make test`
.PHONY: test
test:
	go test ./...

# `make clean`
.PHONY: clean
clean:
	rm -rf ./build/
	rm -rf ./data/
