# Pass recipe arguments through as "$@" so paths with spaces stay intact.
set positional-arguments

bin := "./mediacat"

# Catalog to use; empty means mediacat's default (~/.local/share/mediacat/catalog.db).
# Override per run: just db=/tmp/mc-test.db scan /mnt/nfs/Stash
db := ""

dbflag := if db != "" { "--db " + db } else { "" }

# List recipes
default:
    @just --list --unsorted

# --- development ---

# Build ./mediacat
build:
    go build -o {{bin}} ./cmd/mediacat

# Install to ~/go/bin/mediacat
install:
    go install ./cmd/mediacat

# Run from source without building, e.g. just run dupes --help
run *ARGS:
    go run ./cmd/mediacat {{dbflag}} "$@"

# Run all checks (fmt, vet, test)
check: fmt-check vet test

# Run tests (all packages, or pass packages/flags, e.g. just test ./internal/dupes -run Policy)
test *ARGS:
    go test {{ if ARGS == "" { "./..." } else { "" } }} "$@"

# Run tests with the race detector
test-race *ARGS:
    go test -race {{ if ARGS == "" { "./..." } else { "" } }} "$@"

# Run tests verbosely, filtered by name, e.g. just test-one TestTrashAndUndo
test-one NAME:
    go test -v -run "$1" ./...

# Test coverage summary, with an HTML report in coverage.html
cover:
    go test -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | tail -1
    go tool cover -html=coverage.out -o coverage.html

# Run go vet
vet:
    go vet ./...

# Format code
fmt:
    gofmt -w .

# Check formatting without modifying
fmt-check:
    @test -z "$(gofmt -l .)" || (gofmt -l . && echo "run: just fmt" && exit 1)

# Tidy go.mod and go.sum
tidy:
    go mod tidy

# Remove build and coverage artifacts
clean:
    rm -f {{bin}} coverage.out coverage.html

# --- mediacat (builds first; pass any flags through) ---

# Dashboard: volumes, recent scans, staleness
dashboard: build
    {{bin}} {{dbflag}}

# Catalog media under a path, e.g. just scan /mnt/nfs/Stash --prune
scan *ARGS: build
    {{bin}} {{dbflag}} scan "$@"

# List cataloged media, e.g. just list . --type video --min-res 1080p --sort size --desc
list *ARGS: build
    {{bin}} {{dbflag}} list "$@"

# Show all metadata for one file
info FILE *ARGS: build
    {{bin}} {{dbflag}} info "$@"

# Find duplicates (dry run unless --execute), e.g. just dupes /mnt/nfs/Stash --trash
dupes *ARGS: build
    {{bin}} {{dbflag}} dupes "$@"

# Show the trash on a path's drive; --empty to purge what mediacat put there
trash *ARGS: build
    {{bin}} {{dbflag}} trash "$@"

# List past trash/move/delete batches, or the files in one batch
history *ARGS: build
    {{bin}} {{dbflag}} history "$@"

# Restore a trash or move batch
undo BATCH *ARGS: build
    {{bin}} {{dbflag}} undo "$@"

# Any mediacat command, e.g. just mc --help
mc *ARGS: build
    {{bin}} {{dbflag}} "$@"
