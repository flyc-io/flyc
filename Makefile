.PHONY: test lint build images check-haproxy render-haproxy ansible-fast clean

GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X github.com/flyc-io/flyc/internal/version.Version=$(VERSION)
REGISTRY ?= ghcr.io/flyc-io

test:
	$(GO) vet ./...
	$(GO) test ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o bin/flyc-queue ./queue/cmd/flyc-queue
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o bin/flyc-control ./control/cmd/flyc-control

images:
	docker build -f queue/Dockerfile   --build-arg VERSION=$(VERSION) -t $(REGISTRY)/flyc-queue:$(VERSION) .
	docker build -f control/Dockerfile --build-arg VERSION=$(VERSION) -t $(REGISTRY)/flyc-control:$(VERSION) .

render-haproxy:
	ansible-playbook -i tools/fixtures/inventory.yml tools/render-edge.yml
	ansible-playbook -i tools/fixtures/inventory-all-in-one.yml tools/render-edge.yml \
	  -e render_edge_host=box -e render_queue_host=box -e build_dir=$(CURDIR)/build/edge-aio

check-haproxy: render-haproxy
	haproxy -c -f build/edge/haproxy.cfg
	haproxy -c -f build/edge-aio/haproxy.cfg

# Accélérateur optionnel du playbook (voir tools/ansible-env.sh). Installé dans le dépôt, le
# Python du système n'est pas touché.
ansible-fast:
	python3 -m pip install --quiet --upgrade --target .ansible/mitogen mitogen
	@echo "Mitogen installé. tools/add-node.sh l'utilise automatiquement ;"
	@echo "à la main : source tools/ansible-env.sh puis ansible-playbook …"

clean:
	rm -rf bin build
