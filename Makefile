.PHONY: build test vet cross
build:
	go build -trimpath -o wbot ./cmd/wbot

test:
	go test -race -timeout 60s ./...

vet:
	go vet ./...

cross:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist/wbot_darwin_arm64 ./cmd/wbot
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o dist/wbot_darwin_amd64 ./cmd/wbot
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/wbot_linux_arm64 ./cmd/wbot
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/wbot_linux_amd64 ./cmd/wbot
