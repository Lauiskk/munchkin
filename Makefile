# Munchkin — Makefile
# Alvos de `gates` provam mecanicamente os critérios eliminatórios do §14.

SHELL := /bin/bash
GO    ?= go
GOFMT ?= gofmt
PKG   := ./...

# Pacotes onde dinheiro circula: ponto flutuante é proibido (§5.1, eliminatório)
MONEY_PATHS := internal/domain internal/app internal/adapter/postgres internal/adapter/http/dto

.DEFAULT_GOAL := help

## help: lista os alvos disponíveis
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | column -t -s ':'

## up: sobe o ambiente local (docker compose)
up:
	@docker compose up -d --build
	@echo "✓ ambiente no ar — API em http://localhost:8080, Keycloak em http://localhost:8180"

## down: derruba o ambiente local
down:
	@docker compose down

## logs: acompanha os logs do ambiente
logs:
	@docker compose logs -f --tail=100

## ps: estado dos serviços
ps:
	@docker compose ps

## migrate-up: aplica as migrations pendentes
migrate-up:
	@docker compose run --rm migrate migrate up

## migrate-down: reverte uma migration
migrate-down:
	@docker compose run --rm migrate migrate down

## migrate-down-all: reverte todas as migrations (destrutivo)
migrate-down-all:
	@docker compose run --rm -e MIGRATE_CONFIRM_DESTRUCTIVE=yes migrate migrate down-all

## migrate-status: versão corrente do schema
migrate-status:
	@docker compose run --rm migrate migrate status

## build: compila os binários
build:
	@if [ "$$($(GO) list ./cmd/... 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· nenhum binário ainda, build ocioso"; exit 0; fi; \
	$(GO) build -o bin/api ./cmd/api && echo "✓ bin/api"

## tidy: sincroniza go.mod e go.sum
tidy:
	$(GO) mod tidy

## test: testes unitários
test:
	@if [ "$$($(GO) list $(PKG) 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· nenhum pacote Go ainda, teste ocioso"; exit 0; fi; \
	$(GO) test $(PKG)

## test-race: testes unitários com detector de corrida
test-race:
	@if [ "$$($(GO) list $(PKG) 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· nenhum pacote Go ainda, teste ocioso"; exit 0; fi; \
	$(GO) test -race $(PKG)

## test-integration: testes com infraestrutura real em containers
test-integration:
	$(GO) test -race -tags=integration -count=1 -timeout=20m ./tests/integration/...

## test-concurrency: cenários de concorrência com múltiplos processos
test-concurrency:
	$(GO) test -race -tags=integration -count=1 -timeout=20m ./tests/concurrency/...

## test-recovery: cenários de interrupção e recuperação
test-recovery:
	$(GO) test -race -tags=integration -count=1 -timeout=20m ./tests/recovery/...

## lint: golangci-lint
lint:
	@if [ "$$($(GO) list $(PKG) 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· nenhum pacote Go ainda, lint ocioso"; exit 0; fi; \
	golangci-lint run

# ---------------------------------------------------------------------------
# Gates — cada um corresponde a um critério eliminatório do enunciado
# ---------------------------------------------------------------------------

## gates: roda todos os gates dos critérios eliminatórios
gates: gate-fmt gate-vet gate-deps gate-no-float gate-domain-pure gate-fiber-ctx
	@echo "✓ todos os gates passaram"

## gate-fmt: código formatado com gofmt (§15)
gate-fmt:
	@out=$$($(GOFMT) -l . 2>/dev/null); \
	if [ -n "$$out" ]; then echo "✗ arquivos fora do gofmt:"; echo "$$out"; exit 1; fi; \
	echo "✓ gofmt"

## gate-vet: go vet limpo (§15)
gate-vet:
	@if [ "$$($(GO) list $(PKG) 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· nenhum pacote Go ainda, gate ocioso"; exit 0; fi; \
	$(GO) vet $(PKG) && echo "✓ go vet"

## gate-deps: go.mod e go.sum sincronizados com o código (§15: dependências reproduzíveis)
gate-deps:
	@cp go.mod go.mod.gatebak; cp go.sum go.sum.gatebak 2>/dev/null || true; \
	$(GO) mod tidy; \
	status=0; \
	if ! diff -q go.mod go.mod.gatebak >/dev/null 2>&1 || ! diff -q go.sum go.sum.gatebak >/dev/null 2>&1; then \
	  echo "✗ go.mod/go.sum fora de sincronia — rode 'go mod tidy' e versione o resultado"; \
	  status=1; \
	fi; \
	mv go.mod.gatebak go.mod; mv go.sum.gatebak go.sum 2>/dev/null || true; \
	if [ $$status -eq 0 ]; then echo "✓ dependências sincronizadas"; fi; \
	exit $$status

## gate-no-float: nenhum ponto flutuante onde circula dinheiro (§5.1, eliminatório)
gate-no-float:
	@hits=$$(grep -rnE '\bfloat(32|64)\b|ParseFloat|FormatFloat' \
	  $(MONEY_PATHS) --include='*.go' 2>/dev/null || true); \
	if [ -n "$$hits" ]; then \
	  echo "✗ ponto flutuante em caminho de dinheiro (§5.1 é eliminatório):"; \
	  echo "$$hits"; exit 1; fi; \
	echo "✓ sem float no caminho do dinheiro"

## gate-domain-pure: domínio sem dependência de infraestrutura (§4)
gate-domain-pure:
	@if [ "$$($(GO) list ./internal/domain/... 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· domínio ainda vazio, gate ocioso"; exit 0; fi; \
	deps=$$($(GO) list -deps ./internal/domain/... 2>/dev/null \
	  | grep -E 'gofiber|gorm\.io|go\.uber\.org/fx|aws-sdk-go' || true); \
	if [ -n "$$deps" ]; then \
	  echo "✗ domínio importa infraestrutura (§4):"; echo "$$deps"; exit 1; fi; \
	echo "✓ domínio puro"

##: nenhum documento de trabalho versionado
gate-private-docs:
	@hits=$$(git ls-files | grep -E '^(GUIA-PESSOAL\.md|AGENTS\.md|CLAUDE\.md|PATTERNS\.md|PATTERNS/|docs/(CHALLENGE\.md|adr/|checkpoints/|sdd/|templates/))' || true); \
	if [ -n "$$hits" ]; then \
	  echo "✗ documento de trabalho versionado (deve permanecer local):"; \
	  echo "$$hits"; exit 1; fi; \
	echo "✓ nenhum documento de trabalho versionado"

## gate-fiber-ctx: handlers usam c.UserContext(), nunca c.Context()
# Exceção única, marcada com `gate:allow-fiber-ctx` na mesma linha: o middleware
# que deriva o context.Context da aplicação precisa ler o do fasthttp uma vez.
# A isenção fica visível no código e é encontrável por grep.
gate-fiber-ctx:
	@hits=$$(grep -rn 'c\.Context()' internal/adapter/http --include='*.go' 2>/dev/null \
	  | grep -v 'gate:allow-fiber-ctx' || true); \
	if [ -n "$$hits" ]; then \
	  echo "✗ handler usando c.Context() em vez de c.UserContext():"; \
	  echo "$$hits"; exit 1; fi; \
	echo "✓ propagação de contexto"

.PHONY: help up down logs ps migrate-up migrate-down migrate-down-all migrate-status build tidy test test-race test-integration test-concurrency \
        test-recovery lint gates gate-fmt gate-vet gate-no-float \
        gate-domain-pure gate-fiber-ctx gate-deps
