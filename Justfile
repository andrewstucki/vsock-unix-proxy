default: help

LOCAL_DOCKER_TAG := "vsock-unix-proxy:latest"

# Display help message
help:
    @echo "Available recipes:"
    @echo "  build    - Build the docker image"
    @echo "  test     - Run tests tests"

# Build the docker image
build:
    @echo "Building {{LOCAL_DOCKER_TAG}}..."
    docker build . -t {{LOCAL_DOCKER_TAG}}

# Run tests
test:
    @echo "Running tests..."
    @go test -C test -c -o test-runner
    @cd test && linuxkit build test.yml --docker && mv test-kernel test-kernel.gz && gunzip test-kernel.gz
    @cd test && codesign --force --sign "-" --entitlements vz.entitlements test-runner && ./test-runner -test.v