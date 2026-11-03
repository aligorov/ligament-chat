VERSION ?= v0.1.0
BUILD_DATE ?= $(shell date +%F)
IMAGE ?= aligorov/ligament_chat

.PHONY: test vet build docker-build docker-push

test:
	go test -v ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath \
		-ldflags="-w -s -X main.Version=$(VERSION) -X main.BuildDate=$(BUILD_DATE)" \
		-o bin/ligament-chat ./cmd/ligament-chat

docker-build:
	docker buildx build --platform linux/amd64 --load \
		-t ligament-chat:latest \
		-t ligament-chat:$(VERSION) \
		-t $(IMAGE):latest \
		-t $(IMAGE):$(VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg VERSION=$(VERSION) .

docker-push:
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest
