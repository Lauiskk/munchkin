# Munchkin — processamento distribuído de apostas

Serviço em Go que movimenta carteiras de jogadores por HTTP e por SQS com
garantias equivalentes, sob entrega *at-least-once*, com várias instâncias em
execução e falhas entre as etapas do processamento.

As decisões técnicas, as interpretações adotadas e as limitações conhecidas
estão em [`ARCHITECTURE.md`](ARCHITECTURE.md).

---

## Estado atual

> Atualizado a cada etapa. ✅ significa implementado **e** verificado; 🟡
> significa implementado com verificação pendente; o que não está marcado não
> existe ainda.

| Etapa | Entrega | Estado |
|---|---|---|
| 00 | Fundação: módulo, gates dos critérios eliminatórios, CI | ✅ |
| 01 | Composição com Fx, Fiber, erros padronizados, `/health/live` | ✅ |
| 02 | Keycloak, cache de JWKS, middleware de autenticação | ✅ |
| 03 | Postgres, ciclo de vida, `/health/ready` | ✅ |
| 04 | Migrations versionadas e schema com as constraints | ✅ |
| 05 | `Money` | ✅ |
| 06 | Agregados de domínio | ✅ |
| 07 | Abertura de carteira | ✅ |
| 08 | Operação financeira e idempotência | ✅ |
| 09 | Reversões e referências pendentes | ⬜ |
| 10 | Outbox e publicação | ⬜ |
| 11 | Consumidor SQS e inbox | ⬜ |
| 12 | Consultas e reconciliação | ⬜ |
| 13 | Observabilidade | ⬜ |
| 14 | Documentação de API (Swagger) | ⬜ |
| 15 | Testes de integração com infraestrutura real | ⬜ |
| 16 | Concorrência e recuperação | ⬜ |

---

## Pré-requisitos

| Ferramenta | Versão | Observação |
|---|---|---|
| Go | 1.27.1 | mesma versão declarada em `go.mod` e no Dockerfile |
| Docker | 24+ | com Docker Compose v2 |
| `make` | qualquer | |

Nada além disso: Postgres, Keycloak e LocalStack sobem em container.

## Variáveis de ambiente

```sh
cp .env.example .env
```

O `.env.example` traz valores locais de exemplo, sem segredo real. Nunca
versione o `.env` — há um job de CI que barra isso.

## Como rodar

```sh
cp .env.example .env
docker compose up --build      # ou: make up
```

Sobe o Keycloak com o realm já importado e a aplicação. A imagem da aplicação é
distroless, roda como usuário não privilegiado, com sistema de arquivos somente
leitura e sem capacidade nenhuma.

Para desenvolver com a aplicação fora do container:

```sh
docker compose up -d keycloak
AUTH_ISSUER=http://localhost:8180/realms/munchkin \
AUTH_AUDIENCE=munchkin-api \
make build && ./bin/api
```

Se a porta 8080 já estiver ocupada na sua máquina, ajuste `API_HOST_PORT` no
`.env` — ela é a porta publicada no host, distinta de `HTTP_PORT`, que é a porta
em que a aplicação escuta dentro do container.

Verificação rápida:

```sh
curl -s localhost:8080/health/live
# {"status":"alive"}

curl -s localhost:8080/health/ready
# {"status":"ready","checks":{"postgres":"ok"}}

curl -s -D- localhost:8080/health/live | grep -i x-correlation
# X-Correlation-Id: 01a083fe-4e88-764c-957a-0dc54a549dde
```

Toda resposta carrega `X-Correlation-Id`. Enviar o cabeçalho na requisição
propaga o seu identificador — desde que ele seja alfanumérico, com `-`, `_` ou
`.`, e no máximo 64 caracteres; fora disso o serviço gera o próprio, para que
ninguém consiga forjar registros no log estruturado.

Erros seguem sempre o mesmo formato:

```json
{
  "code": "VALIDATION_ERROR",
  "message": "a requisição contém campos inválidos",
  "fields": [{ "field": "money.amount", "message": "...", "code": "INVALID_FORMAT" }],
  "correlationId": "01a083fe-4e9b-7890-a53b-c2ba55da3806"
}
```

## Autenticação

Toda rota de negócio exige um access token do IdP. Só `/health/live` e
`/health/ready` são públicas.

O ambiente local sobe um Keycloak já provisionado, com o realm importado a cada
subida — os segredos abaixo valem apenas neste compose e não têm valor fora dele.

| Cliente | `provider_id` | Escopos | Para quê |
|---|---|---|---|
| `provider-a` | `provider-a` | `wagering:write`, `wagering:read` | provedor de jogos |
| `provider-b` | `provider-b` | `wagering:write`, `wagering:read` | provar isolamento entre provedores |
| `wallet-admin` | — | `wallets:admin` | abertura interna de carteira |
| `provider-c-sem-escopo` | `provider-c` | nenhum | provar que autenticado não é autorizado |

Obtendo um token e chamando a API:

```sh
TOKEN=$(curl -s -X POST \
  http://localhost:8180/realms/munchkin/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=provider-a \
  -d client_secret=local-only-provider-a | jq -r .access_token)

curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/wallets
```

O `providerId` de qualquer operação vem **do token**, nunca do corpo: uma
requisição cujo corpo discorde do token é recusada.

Uma rota inexistente responde 401 a quem não se identificou e 404 a quem se
identificou — 404 para anônimo contaria quais caminhos existem.

## Testes

```sh
make test               # unitários
make test-race          # unitários com detector de corrida
make gates              # gates dos critérios eliminatórios
make lint

make test-integration   # infraestrutura real em container
make test-concurrency   # 50 envios paralelos, disputa 80+80, múltiplos processos
make test-recovery      # interrupção entre commit e remoção, publishers concorrentes
```

Os três últimos rodam sob a build tag `integration`, então `go test ./...` não
os inclui. Hoje `make test-integration` exige o Keycloak no ar
(`docker compose up -d keycloak`); a partir da etapa 15 a infraestrutura sobe e
desce sozinha via `testcontainers`.

`make test-concurrency` e `make test-recovery` ainda não têm cenários — as
suítes chegam nas etapas 15 e 16.

## Gates

`make gates` prova mecanicamente as condições que não podem ser violadas:

| Gate | O que prova |
|---|---|
| `gate-no-float` | nenhum `float` onde circula dinheiro |
| `gate-domain-pure` | domínio sem dependência de infraestrutura |
| `gate-fiber-ctx` | handlers propagam `context.Context` corretamente |
| `gate-fmt`, `gate-vet` | formatação e análise estática |

Cada gate foi verificado **vermelho** antes de ser aceito verde — gate que nunca
dispara não prova nada.

## Portas

| Serviço | Porta |
|---|---|
| API | 8080 (`API_HOST_PORT`) |
| Postgres | 5440 |
| Keycloak | 8180 |
| LocalStack | 4576 |
| Prometheus | 9091 |
| Grafana | 3000 |

Ajustáveis pelo `.env`. LocalStack, Prometheus e Grafana entram nas próximas
etapas — hoje o compose sobe Keycloak, PostgreSQL e a aplicação.

## Exemplos de chamada

```sh
# token do serviço interno (escopo wallets:admin)
ADMIN=$(curl -s -X POST \
  http://localhost:8180/realms/munchkin/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=wallet-admin \
  -d client_secret=local-only-wallet-admin | jq -r .access_token)

# abre a carteira
curl -s -X POST localhost:8080/wallets \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
       "initialBalance":{"amount":"1000.00","currency":"BRL"}}'
```

```json
201 {"id":"0192f291-...","playerId":"0192f28f-...",
     "balance":{"amount":"1000.00","currency":"BRL"},"version":1}
```

Uma abertura com saldo positivo grava, **no mesmo commit**: a carteira, a
transação `OPENING` em `PROCESSED`, o lançamento de crédito e dois registros de
outbox. Saldo inicial zero grava apenas a carteira.

```sh
curl -s localhost:8080/wallets/$WALLET_ID -H "Authorization: Bearer $ADMIN"
```

Erros trazem o campo apontado, e todos de uma vez:

```json
400 {
  "code": "VALIDATION_ERROR",
  "fields": [
    { "field": "playerId", "code": "INVALID_FORMAT", "message": "..." },
    { "field": "initialBalance.currency", "code": "INVALID_VALUE", "message": "..." }
  ],
  "correlationId": "01a086a2-..."
}
```

| Situação | Código |
|---|---|
| Criada | 201 |
| Entrada inválida | 400 com `fields` |
| Sem credencial | 401 |
| Sem o escopo `wallets:admin` | 403 |
| Carteira inexistente | 404 |
| Jogador já tem carteira nessa moeda | 409 |

## Operação financeira

```sh
PA=$(curl -s -X POST \
  http://localhost:8180/realms/munchkin/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=provider-a \
  -d client_secret=local-only-provider-a | jq -r .access_token)

curl -s -X POST localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PA" \
  -H "Idempotency-Key: provider-a:transaction-123" \
  -H 'Content-Type: application/json' \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123",
       "playerId":"0192f28f-...","walletId":"0192f291-...",
       "roundId":"round-987","gameId":"fortune-chimp",
       "kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
```

```json
200 {"transactionId":"0192f298-...","status":"PROCESSED",
     "balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":false}
```

O mesmo envio repetido devolve `"idempotentReplay": true` com o **mesmo saldo**
— o observado no processamento original, mesmo que a carteira já tenha se
movimentado depois.

| Situação | Código | Corpo |
|---|---|---|
| Processada, ou replay | 200 | `status`, `balance`, `idempotentReplay` |
| Entrada inválida, ou `Idempotency-Key` ausente | 400 | `VALIDATION_ERROR` com `fields` |
| Sem credencial | 401 | `UNAUTHORIZED` |
| Sem escopo, ou `providerId` divergente do token | 403 | `FORBIDDEN` |
| Chave reutilizada com outro conteúdo, ou operação com outra chave | 409 | `CONFLICT` |
| Recusa de negócio | 422 | `status: REJECTED` com `failureCode` |

Consultas, restritas ao provedor do token:

```sh
curl -s localhost:8080/wagering/transactions/$TX_ID -H "Authorization: Bearer $PA"
curl -s localhost:8080/providers/provider-a/wagering/transactions/transaction-123 \
  -H "Authorization: Bearer $PA"
```

Transação de outro provedor responde **404**, não 403 — um 403 confirmaria que
ela existe.

## Migrations

```sh
make migrate-up          # aplica as pendentes
make migrate-down        # reverte uma
make migrate-status      # versão corrente e se o banco está sujo
make migrate-down-all    # reverte todas (destrutivo)
```

Pares `.up.sql`/`.down.sql` versionados em `migrations/`, **embarcados no
binário**: não há como o container subir com uma versão do código e outra do
schema. O `docker compose up` aplica as migrations antes de a aplicação subir,
num serviço próprio que recebe as credenciais do dono do schema — a aplicação
nunca as vê.

Migration já aplicada nunca é editada: corrige-se com uma nova. Se uma migração
for interrompida no meio, o banco fica marcado como sujo e o executor recusa
operar até que alguém confira o schema e resolva com `migrate force <versao>`.

## Banco de dados

A aplicação conecta como `munchkin_app`, que **não é dono** do schema. As
migrations rodam como o dono. A separação existe porque, em PostgreSQL, o dono
de uma tabela tem privilégio por *ownership*: revogar `UPDATE` e `DELETE` dele
não teria efeito, e a imutabilidade do ledger imposta pelo banco deixaria de
existir.

As invariantes financeiras vivem no schema, não no código — um caminho de
aplicação com defeito é recusado pelo banco:

| Invariante | Como é imposta |
|---|---|
| Saldo nunca negativo | `CHECK (balance_minor >= 0)` |
| Uma carteira por jogador e moeda | índice único `(player_id, currency)` |
| Moeda da movimentação igual à da carteira | chave estrangeira composta `(wallet_id, currency)` |
| Operação financeira única por provedor | índice único `(provider_id, external_transaction_id)` |
| Chave de idempotência única por provedor | índice único `(provider_id, idempotency_key)` |
| Um crédito inicial por carteira | índice único parcial em `kind = 'OPENING'` |
| No máximo uma reversão bem-sucedida por referência | índice único parcial em `status='PROCESSED' AND kind IN ('REFUND','ROLLBACK')` |
| Um lançamento por transação e carteira | índice único `(wallet_id, transaction_id)` |
| Aritmética do lançamento fecha a conta | `CHECK` de `balance_after = balance_before ± amount` |
| Ledger append-only | gatilho recusando `UPDATE`/`DELETE`/`TRUNCATE` **e** privilégio revogado |
| Instantâneo do evento imutável | gatilho permitindo alterar só o controle de publicação |

`GET /health/ready` reflete o estado real da dependência:

```sh
curl -s localhost:8080/health/ready
# {"status":"ready","checks":{"postgres":"ok"}}

docker compose stop postgres
curl -s localhost:8080/health/ready
# {"status":"not_ready","checks":{"postgres":"failing"}}   (HTTP 503)

curl -s localhost:8080/health/live
# {"status":"alive"}   (segue 200 — vivacidade não depende do banco)
```
