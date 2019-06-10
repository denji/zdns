all: lint test install

test:
	go test ./...

vet:
	go vet ./...

golint: install-tools
	golint ./...

errcheck: install-tools
	errcheck ./...

staticcheck: install-tools
	staticcheck ./...

fmt:
	bash -c "diff --line-format='%L' <(echo -n) <(gofmt -d -s .)"

lint: fmt vet golint errcheck staticcheck

install-tools:
	cd tools; \
		go list -tags tools -f '{{range $$i := .Imports}}{{printf "%s\n" $$i}}{{end}}' | xargs go install

install-embedded:
	env GOOS=linux GOARCH=mipsle go install -ldflags '-s -w' ./...
	upx -q $(GOPATH)/bin/linux_mipsle/zdns

install:
	go install ./...
