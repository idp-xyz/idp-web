# idp-web build and test entry points.
.PHONY: build vet test image

build:
	go build -o bin/ ./cmd/...

vet:
	gofmt -l . | (! grep .) && go vet ./...

# ./... includes tests/deploy: the Kubernetes manifests against the code (the egress policy against internal/addr)
test: vet
	go test -race ./...

# the image, built offline (a static binary on the host, then docker build --network none)
image:
	bash deploy/build.sh
