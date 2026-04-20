_default:
  just --list --alias-style left --list-heading ''

set dotenv-load
set dotenv-path := ".env"
set quiet

lambdas := "scheduler receiver dispatcher"
tier := `terraform -chdir=terraform workspace show`

# go
# -------------------------------------------------------------------
alias b := build
[doc("build all lambda binaries")]
[group('go')]
build:
  #!/usr/bin/env bash
  set -euo pipefail
  for name in {{ lambdas }}; do
    echo "building $name..."
    mkdir -p bin/$name
    GOARCH=arm64 GOOS=linux go build -ldflags="-s -w" -o bin/$name/bootstrap ./cmd/$name/
  done

alias t := test
[doc("run all tests")]
[group('go')]
test:
  go test ./...

alias r := run
[doc("run scheduler locally")]
[group('go')]
run:
  go run ./cmd/scheduler/

# terraform
# -------------------------------------------------------------------
alias d := deploy
[doc("build all lambdas and deploy with terraform")]
[group('terraform')]
deploy: build
  terraform -chdir=terraform apply -var-file=var.{{ tier }}.tfvars

[doc("select or create a workspace — just workspace staging")]
[group('terraform')]
workspace tier:
  terraform -chdir=terraform workspace select {{ tier }} || terraform -chdir=terraform workspace new {{ tier }}
