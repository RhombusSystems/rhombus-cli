.PHONY: generate build install clean

generate:
	go run ./codegen/ openapi.json cmd/generated/

build: generate
	go build -o rhombus .

install: build
	cp rhombus /usr/local/bin/rhombus

clean:
	rm -f rhombus
	rm -f cmd/generated/*.go
