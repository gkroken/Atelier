# Targets print one line per step on success and only failures otherwise.
# Full output is kept in .logs/.

.PHONY: check test build vet fmt clean

check:
	@scripts/check.sh

test:
	@scripts/test.sh

build:
	@scripts/run-quiet.sh build -- go build -o atelier ./cmd/atelier

vet:
	@scripts/run-quiet.sh vet -- go vet ./...

fmt:
	@gofmt -w $$(git ls-files --cached --others --exclude-standard '*.go')

clean:
	@rm -rf .logs atelier
