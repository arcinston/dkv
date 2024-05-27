run:
	@go run cmd/globaldb.go

build:
	@go build -o ./bin/main cmd/globaldb.go

cli:
	@go run cmd/globaldb.go
