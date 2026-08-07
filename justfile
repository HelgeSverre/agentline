BINARY := "agentline"
CMD := "./cmd/agentline"

# Show available recipes
default:
    @just --list --unsorted

# === Build ===

# Build the binary into ./bin
[group('build')]
build:
    go build -o bin/{{BINARY}} {{CMD}}

# Run from source, passing args through
[group('build')]
run *ARGS:
    go run {{CMD}} {{ARGS}}

# Install into GOPATH/bin
[group('build')]
install:
    go install {{CMD}}

# Remove the installed binary
[group('build')]
uninstall:
    rm -f "$(go env GOPATH)/bin/{{BINARY}}"

# Remove build artifacts
[group('build')]
clean:
    rm -rf bin

# === QA ===

# Format all Go source files
[group('qa')]
fmt:
    gofmt -w .

# Vet + verify formatting is clean
[group('qa')]
lint:
    go vet ./...
    @test -z "$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

# Run all tests with race detector
[group('qa')]
test:
    go test -race ./...

# Full gate: lint + test
[group('qa')]
check: lint test
