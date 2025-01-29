update:
  go get -u
  go mod tidy -v

compile:
  go build -o compare_deployed_secrets.exe main.go
  GOOS=darwin GOARCH=arm64 go build -o compare_deployed_secrets main.go