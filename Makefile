SHELL := /bin/bash

REPO = registry.cn-shanghai.aliyuncs.com
NAMESPACE = jibutech
IMG_NAME = imagepuller
VERSION ?= $(shell git rev-parse --abbrev-ref HEAD).$(shell git rev-parse --short HEAD)
IMAGE_TAG_BASE = $(REPO)/$(NAMESPACE)/$(IMG_NAME)
IMG = $(IMAGE_TAG_BASE):$(VERSION)

build: ## Build manager binary.
	go build -o manager main.go
docker-push: ## Push docker image with the manager.
	docker buildx build --platform linux/amd64 -t ${IMG} . --push 
docker-pushx: ## Push docker image with the manager.
	docker buildx build --platform linux/amd64,linux/arm64 -t ${IMG} . --push 