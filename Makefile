.PHONY: build install uninstall test vet fmt clean integration

BIN := svidoq
PREFIX ?= $(HOME)/.local
PKG := github.com/iamnikolie/svidoq/cmd

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG).version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

install: build
	mkdir -p $(PREFIX)/bin
	ln -sf $(CURDIR)/$(BIN) $(PREFIX)/bin/$(BIN)
	@echo "linked $(PREFIX)/bin/$(BIN) -> $(CURDIR)/$(BIN)"

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

test:
	go test ./...

# Integration tests against a throwaway MySQL. They are skipped by `make test`.
integration:
	docker rm -f svidoq-test >/dev/null 2>&1 || true
	docker run -d --name svidoq-test -e MYSQL_ROOT_PASSWORD=testpw \
		-e MYSQL_DATABASE=billing -p 13399:3306 mysql:8.4 >/dev/null
	@echo "waiting for mysql..."
	@until docker exec svidoq-test mysqladmin ping -uroot -ptestpw --silent >/dev/null 2>&1; do sleep 2; done
	@docker exec -i svidoq-test mysql -uroot -ptestpw billing <<< \
		"CREATE TABLE IF NOT EXISTS invoices (id INT PRIMARY KEY AUTO_INCREMENT, client VARCHAR(64), total DECIMAL(10,2)); \
		 INSERT INTO invoices (client,total) VALUES ('a',1),('b',2);" 2>/dev/null || true
	SVIDOQ_TEST_DSN='root:testpw@tcp(127.0.0.1:13399)/billing?parseTime=true' go test ./... -run Integration -v
	docker rm -f svidoq-test >/dev/null

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN)
	rm -rf dist
