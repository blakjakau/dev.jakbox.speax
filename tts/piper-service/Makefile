# Piper-Service Makefile

BINARY_NAME=piper-service
LIB_DIR=./lib
ESPEAK_DATA=./piper/espeak-ng-data
PORT=4410

.PHONY: all build run clean test

all: build

build:
	@echo "Building $(BINARY_NAME)..."
	go build -o $(BINARY_NAME) .

run: build
	@echo "Starting $(BINARY_NAME) on port $(PORT)..."
	LD_LIBRARY_PATH=$(LIB_DIR) ./$(BINARY_NAME) -port $(PORT) -espeak $(ESPEAK_DATA)

clean:
	@echo "Cleaning up..."
	rm -f $(BINARY_NAME)
	rm -rf dist/
	rm -f *.o

test:
	@echo "Running tests..."
	go test ./...

# Help target to show usage
help:
	@echo "Usage:"
	@echo "  make build    - Compile the piper-service binary"
	@echo "  make run      - Build and run the service with correct LD_LIBRARY_PATH"
	@echo "  make clean    - Remove build artifacts"
	@echo "  make test     - Run Go tests"
