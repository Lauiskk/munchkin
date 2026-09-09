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

## fuzz: fuzzing do parser monetário (padrão: 60s; use FUZZTIME para mudar)
fuzz:
	@$(GO) test -run FuzzParseIdaEVolta -fuzz FuzzParseIdaEVolta \
	  -fuzztime=$${FUZZTIME:-60s} ./internal/domain/money/

## lint: golangci-lint
lint:
	@if [ "$$($(GO) list $(PKG) 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· nenhum pacote Go ainda, lint ocioso"; exit 0; fi; \
	golangci-lint run

# ---------------------------------------------------------------------------
# Gates — cada um corresponde a um critério eliminatório do enunciado
# ---------------------------------------------------------------------------

## gates: roda todos os gates dos critérios eliminatórios
gates: gate-fmt gate-vet gate-deps gate-no-float gate-domain-pure gate-app-pure gate-publish-after-commit gate-fiber-ctx gate-failure-codes gate-hash-fields gate-env-documented
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
# O comentário de linha é removido antes da segunda passada: o próprio código que
# documenta a proibição menciona os termos proibidos, e um gate que reprova a
# documentação da regra vira um gate que alguém desliga. Comentário de bloco
# ainda produziria falso positivo — que é falha para o lado seguro.
gate-no-float:
	@hits=$$(grep -rnE '\bfloat(32|64)\b|ParseFloat|FormatFloat' \
	  $(MONEY_PATHS) --include='*.go' 2>/dev/null \
	  | sed 's|//.*||' \
	  | grep -E '\bfloat(32|64)\b|ParseFloat|FormatFloat' || true); \
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

## gate-failure-codes: catálogo de falhas do código igual ao do ARCHITECTURE (§7)
# O §7 exige que todo failureCode seja estável e DOCUMENTADO. Documentação que
# desatualiza em silêncio é pior que documentação nenhuma: ela descreve um
# sistema que não existe. Este gate compara as duas listas.
gate-failure-codes:
	@codigo=$$(grep -oE 'FailureCode = "[A-Z_]+"' internal/domain/wagering/kind.go \
	  | grep -oE '"[A-Z_]+"' | tr -d '"' | sort -u); \
	doc=$$(grep -oE '^\| `[A-Z_]+` \|' ARCHITECTURE.md | grep -oE '[A-Z_]+' | sort -u); \
	if [ "$$codigo" != "$$doc" ]; then \
	  echo "✗ o catálogo de códigos de falha diverge entre o código e o ARCHITECTURE:"; \
	  diff <(echo "$$codigo") <(echo "$$doc") | sed 's/^/    /'; exit 1; fi; \
	echo "✓ catálogo de falhas documentado"

## gate-app-pure: casos de uso sem dependência de adaptador
# O caso de uso declara as interfaces de que precisa; o adaptador se adapta. A
# direção oposta faz a regra de negócio depender do formato do banco e do
# framework web — foi o que aconteceu ao escrever a abertura de carteira, e o
# gate existe porque vai acontecer de novo.
gate-app-pure:
	@if [ "$$($(GO) list ./internal/app/... 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· camada de aplicação ainda vazia, gate ocioso"; exit 0; fi; \
	deps=$$($(GO) list -deps ./internal/app/... 2>/dev/null \
	  | grep -E 'munchkin/internal/adapter|gofiber|gorm\.io|aws-sdk-go' || true); \
	if [ -n "$$deps" ]; then \
	  echo "✗ caso de uso importa adaptador:"; echo "$$deps" | sed 's/^/    /'; exit 1; fi; \
	echo "✓ casos de uso independentes de adaptador"

## gate-publish-after-commit: quem move dinheiro não alcança o publicador (§14)
##
## Publicar antes do commit é eliminatório. A garantia não é "ninguém faz isso":
## é que o caminho transacional não tem como fazer, porque não enxerga o
## publicador. Gravar na outbox continua permitido — é o que precisa acontecer
## DENTRO da transação; o que fica fora de alcance é quem tira de lá.
gate-publish-after-commit:
	@paths="./internal/app/wagering/... ./internal/app/wallet/..."; \
	if [ "$$($(GO) list $$paths 2>/dev/null | wc -l)" -eq 0 ]; then \
	  echo "· casos de uso financeiros ainda vazios, gate ocioso"; exit 0; fi; \
	deps=$$($(GO) list -deps $$paths 2>/dev/null \
	  | grep -E 'munchkin/internal/app/outbox|munchkin/internal/adapter/sqs' || true); \
	if [ -n "$$deps" ]; then \
	  echo "✗ o caminho transacional alcança o publicador:"; echo "$$deps" | sed 's/^/    /'; exit 1; fi; \
	echo "✓ publicação fora da transação"

## gate-env-documented: variável obrigatória aparece no .env.example e no README
##
## Existe porque o erro aconteceu duas vezes: tornar uma variável obrigatória —
## que é o certo, configuração incompleta tem de recusar a subida — invalidou o
## comando que o README documenta para rodar fora do container, e nas duas vezes
## isso só apareceu quando alguém foi rodar o que estava escrito.
##
## Um leitor encontra a instrução quebrada na primeira página do repositório.
## Disciplina já falhou aqui; agora falha o build.
gate-env-documented:
	@faltando=""; \
	for v in $$(grep -oE 'v\.required\("[A-Z_]+"\)' internal/config/config.go \
	            | sed 's/.*"\(.*\)".*/\1/' | sort -u); do \
	  grep -qE "^$$v=" .env.example || faltando="$$faltando .env.example:$$v"; \
	  grep -qE "(^|[[:space:]])$$v=" README.md || faltando="$$faltando README.md:$$v"; \
	done; \
	if [ -n "$$faltando" ]; then \
	  echo "✗ variável obrigatória não documentada:"; \
	  for f in $$faltando; do echo "    $$f"; done; exit 1; fi; \
	echo "✓ variáveis obrigatórias documentadas"

## gate-hash-fields: campos do hash de idempotência iguais aos do ARCHITECTURE
# O conjunto de campos é contrato: acrescentar um muda a identidade de TODA
# operação já registrada. Documentação que descreve outro conjunto é pior que
# nenhuma — é a terceira deriva de documentação neste projeto, e a segunda a
# virar gate.
gate-hash-fields:
	@codigo=$$(grep -oE '"[a-zA-Z.]+":[[:space:]]+f\.' internal/domain/wagering/fingerprint.go \
	  | grep -oE '"[a-zA-Z.]+"' | tr -d '"' | sort -u); \
	doc=$$(sed -n '/^externalTransactionId, gameId/,/walletId$$/p' ARCHITECTURE.md \
	  | tr ',' '\n' | tr -d ' ' | grep -v '^$$' | sort -u); \
	if [ "$$codigo" != "$$doc" ]; then \
	  echo "✗ os campos do hash divergem entre o código e o ARCHITECTURE:"; \
	  diff <(echo "$$codigo") <(echo "$$doc") | sed 's/^/    /'; exit 1; fi; \
	echo "✓ campos do hash documentados"

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

.PHONY: help fuzz up down logs ps migrate-up migrate-down migrate-down-all migrate-status build tidy test test-race test-integration test-concurrency \
        test-recovery lint gates gate-fmt gate-vet gate-no-float \
        gate-domain-pure gate-app-pure gate-fiber-ctx gate-deps gate-failure-codes \
        gate-hash-fields
