BUILD_TIME=${shell date "+%Y-%m-%d %H:%M:%S"}
GIT_COMMIT=${shell git rev-parse --short HEAD}
VERSION=v$(shell cat VERSION)

generate:
	go generate ./...

build:
	go build -ldflags '-X "main.Version=${VERSION}" -X "main.BuildTime=${BUILD_TIME}" -X "main.GitCommit=${GIT_COMMIT}"' ./
