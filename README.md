# Munchkin — processamento distribuído de apostas

Serviço em Go que movimenta carteiras de jogadores por HTTP e por SQS com
garantias equivalentes, sob entrega *at-least-once*, com várias instâncias em
execução e falhas entre as etapas do processamento.

As decisões técnicas, as interpretações adotadas e as limitações conhecidas
estão em [`ARCHITECTURE.md`](ARCHITECTURE.md).

---

## Índice

| Para | Vá para |
|---|---|
| Subir e usar | [Como rodar](#como-rodar) · [Autenticação](#autenticação) · [Exemplos de chamada](#exemplos-de-chamada) |
| Entender o contrato | [Operação financeira](#operação-financeira) · [Reversões](#reversões) · [Extrato e reconciliação](#extrato-e-reconciliação) · [Contrato da API](#contrato-da-api) |
| Mensageria | [Eventos e filas](#eventos-e-filas) · [Entrada por mensageria](#entrada-por-mensageria) |
| Operar | [Migrations](#migrations) · [Banco de dados](#banco-de-dados) · [Observabilidade](#observabilidade) · [Portas](#portas) |
| Verificar | [Testes](#testes) · [Testes de carga](#testes-de-carga) · [Gates](#gates) |

As decisões de arquitetura — dinheiro, transações, idempotência, locks,
reversões, inbox/outbox, autenticação, shutdown, limitações e trabalho não
concluído — estão no [`ARCHITECTURE.md`](ARCHITECTURE.md).

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
| 09 | Reversões e referências pendentes | ✅ |
| 10 | Outbox e publicação | ✅ |
| 11 | Consumidor SQS e inbox | ✅ |
| 12 | Consultas e reconciliação | ✅ |
| 13 | Observabilidade | ✅ |
| 14 | Documentação de API (OpenAPI) | ✅ |
| 15 | Testes de integração com infraestrutura real | ✅ |
| 16 | Concorrência e recuperação | ✅ |
| 18 | Testes de carga com k6 — *diferencial opcional* | ✅ |
| 19 | Tracing OpenTelemetry, desligado por padrão — *diferencial opcional* | ✅ |

Não há etapa 17: a documentação final não virou checkpoint próprio, e sim parte
de cada um. Grafana, Loki, dashboards e partidas dobradas são os diferenciais
opcionais que **não** foram feitos, e estão declarados no `ARCHITECTURE.md`.

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

Sobe o Keycloak com o realm já importado, o PostgreSQL migrado, o LocalStack com
as filas provisionadas e a aplicação. A imagem da aplicação é distroless, roda
como usuário não privilegiado, com sistema de arquivos somente leitura e sem
capacidade nenhuma.

Para desenvolver com a aplicação fora do container, suba as dependências e
aponte para as portas publicadas no host — que são diferentes das portas
internas usadas pelo compose:

```sh
docker compose up -d keycloak postgres migrate localstack
make build

AUTH_ISSUER=http://localhost:8180/realms/munchkin \
AUTH_AUDIENCE=munchkin-api \
DB_HOST=localhost DB_PORT=5440 DB_NAME=munchkin \
DB_USER=munchkin_app DB_PASSWORD=local-only-app \
AWS_REGION=us-east-1 AWS_ENDPOINT_URL=http://localhost:4576 \
AWS_ACCESS_KEY_ID=local-only-access-key \
AWS_SECRET_ACCESS_KEY=local-only-secret-key \
AWS_EVENTS_QUEUE_URL=http://localhost:4576/000000000000/wager-events.fifo \
AWS_TRANSACTIONS_QUEUE_URL=http://localhost:4576/000000000000/wager-transactions.fifo \
AWS_TRANSACTIONS_DLQ_URL=http://localhost:4576/000000000000/wager-transactions-dlq.fifo \
./bin/api
```

A aplicação recusa subir com configuração incompleta, e a mensagem lista **todas**
as variáveis faltantes de uma vez — não a primeira e depois a seguinte.

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

make test-integration   # PostgreSQL, Keycloak e LocalStack reais, em container
make test-concurrency   # goroutines disputando: 80+80, 50 envios, HTTP x fila
make test-recovery      # três PROCESSOS: kill -9, pendência órfã, reentrega
```

Os três últimos rodam sob a build tag `integration`, então `go test ./...` não
os inclui.

**Não há passo manual.** PostgreSQL, Keycloak e LocalStack sobem e descem
sozinhos, via `testcontainers` — basta o Docker no ar. O PostgreSQL é um por
teste, porque o estado financeiro é o objeto sob prova; o Keycloak e o
LocalStack são compartilhados pela suíte, com isolamento no recurso: cada teste
cria a própria fila, e o realm é imutável durante os testes.

`KEYCLOAK_BASE_URL` aponta a suíte para um IdP já de pé, se você quiser poupar o
tempo de subida dele.

`make test-concurrency` tem oito cenários: disputa 80+80 sobre 100, cinquenta
envios paralelos da mesma aposta, carteiras distintas em paralelo, saldo contra
ledger, dois publicadores sobre a mesma outbox, dois resolvedores sobre a mesma
pendência, a mesma operação por HTTP e por fila, e reconciliação sob
movimentação.

`make test-recovery` sobe **três processos do binário compilado** sobre a mesma
infraestrutura e verifica o que só aparece com instâncias de verdade: cinquenta
envios da mesma aposta distribuídos entre elas, a disputa 80+80 saindo de
processos diferentes, uma pendência retomada por outra instância depois que a
primeira morre com `kill -9`, e reentrega depois do commit sem segundo efeito.

Um `kill -9` deliberado, e não `SIGTERM`: o encerramento ordenado já tem teste
próprio, e o que faltava verificar era o desligamento que **não** dá chance de
limpar nada.

As três suítes com container são pesadas — Postgres, Keycloak e LocalStack, mais
os processos. Numa máquina com outras coisas rodando, containers podem estourar
prazo de subida.

## Testes de carga

Diferencial opcional do §14. `make load-test` roda k6 em container contra a
pilha local — nada a instalar além de Docker.

```sh
docker compose up -d
make load-test
```

**Ambiente da medição abaixo.** Os números descrevem *esta* máquina, não
capacidade de produção: WSL2 sobre Windows, 12 CPUs, 15 GB, Docker Desktop
29.7.2, com **17 containers de outros projetos rodando ao lado**. Toda a pilha
— aplicação, PostgreSQL, Keycloak e LocalStack — na mesma máquina que o gerador
de carga.

**Metodologia.** Três perfis em sequência, nunca simultâneos: rodá-los juntos
misturaria efeitos, e a contenção de uma carteira inflaria a latência das
outras.

| Perfil | VUs | Duração | O que mede |
|---|---:|---:|---|
| Carteiras distintas | 20 | 30s | Vazão do caminho financeiro sem contenção |
| Mesma carteira | 20 | 30s | O custo do lock de linha |
| Replay idempotente | 10 | 20s | O caminho de reenvio |

### Resultado

```
Requisições         37.725          Vazão   129,6 req/s        FALHAS   0

Latência              p50      p95      p99      máx
  carteiras distintas  18ms     39ms     60ms    541ms
  MESMA carteira      103ms    256ms    512ms   2098ms
  replay               58ms     68ms     80ms    127ms

Desfechos    processadas 19.439 · replays 3.408 · recusas 0 · conflitos 0
```

**O preço do lock é visível e esperado:** a mesma operação numa carteira
disputada custa **5,7× mais no p50** e **8,5× no p99**. É o que se paga por não
ter saldo negativo — e é a razão de carteiras distintas seguirem em paralelo.

Recusa por saldo e conflito de idempotência **não** contam como erro: são
desfechos previstos. Só 5xx e falha de rede contam, e não houve nenhum.

**A consistência financeira sobreviveu:** ao fim, 14.834 carteiras e
**zero divergências** entre saldo armazenado e soma do ledger.

### O que a carga encontrou: a outbox é o gargalo

| | |
|---|---:|
| Eventos produzidos pela carga | ~761/s |
| Eventos publicados pelo worker | ~202/s |
| Atraso ao fim da carga | 84s |
| Atraso 200s depois | 270s — o acúmulo **não** drenou |

A causa é aritmética, não defeito: o publicador faz **uma chamada SQS por
evento**, a ~5ms por ida, e o padrão da aplicação é ainda mais conservador —
lote de 50 a cada 2s, ou seja **25 eventos/s**. O compose eleva para lote de 500
a cada 500ms, o que rendeu as 202/s medidas.

Nada se perde: os eventos estão na outbox, duráveis, e saem quando o publicador
alcançar. O que existe é **atraso de integração**, e ele é visível exatamente
porque a métrica `munchkin_outbox_pending_age_seconds` foi feita para isso — ela
mede a idade do pendente mais antigo, e não a contagem, justamente para
distinguir acúmulo temporário de incapacidade.

O próximo passo está declarado no [`ARCHITECTURE.md`](ARCHITECTURE.md):
`SendMessageBatch`, que agrupa até dez mensagens por chamada.

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
| Métricas e documentação | 9092 |
| Prometheus | 9091 |
| Grafana | 3000 |
| Jaeger | 16686 (`JAEGER_UI_PORT`) |

Ajustáveis pelo `.env`. O compose padrão sobe Keycloak, PostgreSQL, LocalStack e
a aplicação; Prometheus e Jaeger ficam atrás de `--profile observability`.

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
| Reversão esperando a referência | 202 | `status: PENDING_REFERENCE` |
| Dependência fora do ar | 503 | `SERVICE_UNAVAILABLE` — **retentável** |
| Prazo da requisição excedido | 504 | `TIMEOUT` — reenvie com a mesma chave |
| Falha inesperada | 500 | `INTERNAL_ERROR`, com `correlationId` |

### Reversões

`REFUND` e `ROLLBACK` usam o mesmo endpoint e exigem
`referenceExternalTransactionId` — o `externalTransactionId` da operação a
reverter, no mesmo provedor. `REFUND` reverte apenas `BET`; `ROLLBACK` reverte
`BET`, `WIN` e `REFUND`.

```sh
curl -s -X POST localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PA" \
  -H "Idempotency-Key: provider-a:refund-123" \
  -H 'Content-Type: application/json' \
  -d '{"providerId":"provider-a","externalTransactionId":"refund-123",
       "playerId":"0192f28f-...","walletId":"0192f291-...",
       "roundId":"round-987","gameId":"fortune-chimp",
       "kind":"REFUND","money":{"amount":"25.00","currency":"BRL"},
       "referenceExternalTransactionId":"transaction-123"}'
```

A reversão tem de concordar com a referência em provedor, jogador, carteira,
moeda, rodada **e valor** — reversão parcial está fora do escopo. Cada
referência aceita **no máximo uma reversão bem-sucedida**; a segunda é recusada
com `REFERENCE_ALREADY_REVERSED`.

Se a referência ainda não chegou, a operação **não é recusada**: é persistida
como `PENDING_REFERENCE` e respondida com **202**. Um worker presente em todas
as instâncias retoma a pendência com backoff exponencial; quando a referência
chega, a reversão conclui e emite os eventos normalmente. Esgotado o limite de
tentativas, ela termina `REJECTED` com `REFERENCE_NOT_FOUND`.

| Recusa | `failureCode` |
|---|---|
| A referência já foi revertida | `REFERENCE_ALREADY_REVERSED` |
| Tipo, provedor, jogador, carteira ou rodada divergentes | `REFERENCE_MISMATCH` |
| Valor diferente do referenciado | `AMOUNT_MISMATCH` |
| A referência terminou sem sucesso | `REFERENCE_NOT_PROCESSED` |
| A referência não chegou no prazo | `REFERENCE_NOT_FOUND` |
| Reverter deixaria a carteira negativa | `REVERSAL_INSUFFICIENT_FUNDS` |

Consultas, restritas ao provedor do token:

```sh
curl -s localhost:8080/wagering/transactions/$TX_ID -H "Authorization: Bearer $PA"
curl -s localhost:8080/providers/provider-a/wagering/transactions/transaction-123 \
  -H "Authorization: Bearer $PA"
```

Transação de outro provedor responde **404**, não 403 — um 403 confirmaria que
ela existe.

## Extrato e reconciliação

```sh
curl -s "localhost:8080/wallets/$WID/ledger?limit=50" -H "Authorization: Bearer $ADMIN"
```

```json
{
  "walletId": "0192f291-...",
  "entries": [
    { "id": "0192f298-...", "transactionId": "0192f298-...",
      "direction": "DEBIT",
      "money":         { "amount": "25.00",   "currency": "BRL" },
      "balanceBefore": { "amount": "1000.00", "currency": "BRL" },
      "balanceAfter":  { "amount": "975.00",  "currency": "BRL" },
      "createdAt": "2026-09-09T12:00:00.000Z" }
  ],
  "nextCursor": "MjAyNi0wOS0wOVQxMjowMDowMFp8MDE5MmYyOTgt..."
}
```

Paginação por **keyset**, do mais recente para o mais antigo, com cursor
**opaco** — não interprete nem construa: passe de volta o `nextCursor` recebido.
A última página não traz `nextCursor`, e é assim que se sabe parar. `limit`
padrão 50, máximo 200.

Um lançamento novo durante a paginação não desloca as páginas seguintes: ele é
mais recente que a posição do cursor e cai fora delas por construção.

```sh
curl -s -X POST "localhost:8080/wallets/$WID/reconciliation" -H "Authorization: Bearer $ADMIN"
```

```json
{
  "walletId": "0192f291-...",
  "storedBalance":     { "amount": "975.00", "currency": "BRL" },
  "calculatedBalance": { "amount": "975.00", "currency": "BRL" },
  "difference":        { "amount": "0.00",   "currency": "BRL" },
  "consistent": true,
  "checkedEntries": 2
}
```

Reconstrói o saldo somando o ledger — incluindo a abertura — e compara com o
armazenado. `difference` é o armazenado **menos** o reconstruído, e sai com
sinal: saldo menor que o ledger é tão divergência quanto o contrário. A
conferência **não altera nada**, e lê os dois valores na mesma instrução, para
que uma movimentação concorrente não produza divergência falsa.

| Situação | Código |
|---|---|
| Extrato ou conferência devolvidos | 200 |
| `cursor` ou `limit` inválidos | 400 com `fields` |
| Sem credencial | 401 |
| Sem escopo `wallets:admin` | 403 |
| Carteira inexistente | 404 |

## Eventos e filas

Três filas FIFO, provisionadas pelo LocalStack na subida:

| Fila | Papel |
|---|---|
| `wager-events.fifo` | Destino dos eventos de saída publicados pela outbox |
| `wager-transactions.fifo` | Entrada de operações por mensageria (etapa 11) |
| `wager-transactions-dlq.fifo` | Destino do redrive da anterior, após 5 recebimentos |

```sh
docker compose exec localstack awslocal sqs list-queues
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localstack:4566/000000000000/wager-events.fifo \
  --max-number-of-messages 10 --visibility-timeout 0
```

**Como o evento sai.** A transação de negócio grava o evento na tabela
`outbox_events`, no mesmo commit do saldo e do ledger. Nada publica ali. Um
worker separado, presente em **todas** as instâncias, reivindica os pendentes,
envia e confirma. Uma interrupção entre enviar e confirmar faz o evento sair de
novo — com o mesmo `eventId`, que é o identificador de deduplicação da fila.

**Contrato de roteamento** de `wager-events.fifo`:

| Atributo | Valor |
|---|---|
| `MessageGroupId` | `aggregateId`, isto é, a carteira — ordena os eventos dela sem serializar carteiras distintas |
| `MessageDeduplicationId` | `eventId` — republicação não vira evento novo |

**Envelope**, comum aos quatro eventos:

```json
{
  "eventId":       "01a086af-04f1-7755-8622-4daf3be069aa",
  "eventType":     "WalletBalanceChanged",
  "aggregateType": "wallet",
  "aggregateId":   "01a086af-04ef-717b-919a-dfebbebe1c87",
  "correlationId": "01a086af-04ee-7a55-8ab1-1f2a6b9f4c31",
  "causationId":   null,
  "occurredAt":    "2026-09-09T17:19:09.197Z",
  "version":       1,
  "data":          { }
}
```

| Evento | Gatilho |
|---|---|
| `WagerTransactionProcessed` | Conclusão bem-sucedida, inclusive `LOSS` |
| `WagerTransactionRejected` | Recusa definitiva por regra de negócio, com `failureCode` |
| `WagerTransactionPendingReference` | Registro da espera por uma referência |
| `WalletBalanceChanged` | Alteração efetiva do saldo — `LOSS` não produz este |

Instantes em RFC 3339 UTC com precisão fixa de milissegundos, no envelope e
dentro do `data`. Dinheiro sempre em string decimal.

### Entrada por mensageria

A mesma operação financeira entra por HTTP e por `wager-transactions.fifo`,
usando o **mesmo caso de uso** e a **mesma decodificação** dos campos. A única
diferença é onde a chave de idempotência viaja: cabeçalho no HTTP, `data` na
mensagem.

```sh
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localstack:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET_ID" --message-deduplication-id msg-123 \
  --message-body '{
    "messageId": "msg-123",
    "type": "WagerTransactionRequested",
    "occurredAt": "2026-09-09T12:00:00.000Z",
    "data": {
      "providerId": "provider-a", "externalTransactionId": "transaction-123",
      "idempotencyKey": "provider-a:transaction-123",
      "playerId": "0192f28f-...", "walletId": "0192f291-...",
      "roundId": "round-987", "gameId": "fortune-chimp",
      "kind": "BET", "money": {"amount": "25.00", "currency": "BRL"}
    }}'
```

| Item | Valor |
|---|---|
| Identidade durável da mensagem | `messageId`, por consumidor |
| Nome do consumidor na inbox | `wager-transactions` |
| Chave de idempotência financeira | `data.idempotencyKey` |
| Visibility timeout | 30s |
| `maxReceiveCount` | 5, então redrive para a DLQ |
| Long polling | 20s |

**O que acontece com a mensagem:**

| Situação | Destino |
|---|---|
| Processada, ou já concluída antes | Removida da fila |
| Recusa de negócio (`REJECTED` com `failureCode`) | Removida — retentar daria a mesma recusa |
| Falha transitória | Mantida; volta a ficar visível e é retentada |
| Envelope inválido, campo irrecuperável, `OPENING` | DLQ imediatamente, com o motivo em atributo |
| Mesmo `messageId` com conteúdo diferente | DLQ — é mensagem inválida, não atualização |

O registro da inbox e o efeito financeiro são gravados **no mesmo commit**, e a
mensagem só é removida depois dele. Uma interrupção entre o commit e a remoção
causa reentrega, e a reentrega encontra a inbox concluída: remove sem
reprocessar.

## Contrato da API

```sh
docker compose up -d
open http://localhost:9092/docs          # interface navegável
curl -s localhost:9092/openapi.yaml      # o documento
```

O contrato está em `api/openapi.yaml`, em OpenAPI 3.0, e é **embarcado no
binário** — não há como servir uma versão e versionar outra. Ele descreve as
nove rotas, os corpos de erro, os doze `failureCode` e os escopos exigidos por
rota.

Fica no mesmo listener das métricas, separado da porta de negócio: o contrato
revela o mapa da API, e num ambiente real essa porta não sai da rede interna.

**Ele não pode divergir do código, e isso não depende de disciplina:**

- `gate-openapi-sync` compara as rotas do roteador com as do documento, nos dois
  sentidos, e os códigos de falha do documento com o catálogo do domínio.
- A suíte de integração levanta a aplicação HTTP inteira e **valida cada
  resposta real** contra o esquema declarado para aquela rota, método e status.
  Um campo renomeado ou um formato que mude quebra o teste.

Os cinco cenários que o §9 exige distinguíveis estão na rota de operação
financeira, com uma tabela dizendo como reconhecer cada um.

Todo status que o serviço pode devolver está declarado, inclusive os que valem
para qualquer rota — 500, 503 e 504. A distinção entre eles não é cosmética:
**500 diz "há um defeito aqui" e não se retenta; 503 diz "tente de novo"**. Um
banco fora do ar responde 503, e o gate exige que toda operação declare pelo
menos o envelope de erro inesperado.

## Observabilidade

**Logs** em JSON no stdout, carregando os identificadores disponíveis —
`correlationId`, `messageId`, `transactionId`, `walletId`, `providerId`. Nunca
credencial, dado sensível ou payload financeiro: log carrega identificador, não
conteúdo. Um gate de CI recusa o build quando um atributo de log usa nome de
conteúdo.

**Métricas** em porta separada da API, porque métrica revela volume de operação
e taxa de recusa — e essa superfície não é a de negócio:

```sh
curl -s localhost:9092/metrics | grep munchkin_
```

| Métrica | Tipo | O que responde |
|---|---|---|
| `munchkin_wager_transactions_total` | counter | Resultados por tipo, status, código de falha e origem |
| `munchkin_idempotent_replays_total` | counter | Duplicatas, por origem |
| `munchkin_wager_processing_duration_seconds` | histogram | Latência do caminho financeiro |
| `munchkin_worker_retries_total` | counter | Retentativas, por worker |
| `munchkin_messages_dead_lettered_total` | counter | DLQ, por categoria de descarte |
| `munchkin_concurrency_conflicts_total` | counter | Disputas perdidas, por tipo |
| `munchkin_outbox_pending_age_seconds` | gauge | Atraso da outbox |
| `munchkin_reconciliation_divergences_total` | counter | Saldo em desacordo com o ledger |

Nenhum rótulo carrega identificador: além de multiplicar as séries até derrubar
o coletor, um `walletId` em rótulo publicaria a lista de carteiras para quem
lesse `/metrics`. Os rótulos são categorias fechadas, conferidas por teste.

O atraso da outbox é a **idade do pendente mais antigo**, não a contagem: mil
eventos recém-gravados não são problema, e um evento parado há uma hora é.

Para ver a coleta funcionando:

```sh
docker compose --profile observability up -d
open http://localhost:9091      # Prometheus, com o alvo munchkin em UP
```

**Tracing** OpenTelemetry, diferencial opcional do §12, **desligado por padrão**.
Sem `TRACING_ENABLED=true` não há exportador, não há goroutine de envio e não há
span alocado por requisição — o provedor é nulo. O padrão é esse porque um
exportador ativo por engano manda dados de operação para um endereço configurado
em algum lugar.

```sh
TRACING_ENABLED=true docker compose --profile observability up -d
open http://localhost:16686     # Jaeger
```

Ligado, um trace cobre a requisição inteira até as consultas do banco:

```
▸ POST /wallets                                   8.81ms
  └ gorm.Create  1.36ms   └ gorm.Create  1.31ms
  └ gorm.Raw     1.04ms   └ gorm.Create  0.57ms   └ gorm.Create  0.31ms
```

O nome do span é o **template** da rota — `GET /wallets/{walletId}`, nunca o
identificador — pela mesma razão que nenhum rótulo de métrica carrega
identificador. Os workers abrem um span por rodada, para que as consultas de
fundo não apareçam órfãs.

Log e trace se apontam: **toda** linha de log carrega `traceId` quando há span, e
o span de servidor carrega `correlation.id`. Sem esse par, quem tem um não acha o
outro. Um `traceparent` recebido no cabeçalho continua o mesmo trace — a
propagação W3C fica registrada mesmo com o tracing desligado, para não quebrar o
trace de quem chamou.

O trace **não atravessa a outbox**: a requisição que grava o evento e a
publicação que acontece depois são dois traces distintos, ligados pelo
`correlationId`. Ligá-los exigiria persistir o `traceparent` numa tabela do
caminho financeiro, e o ganho não paga o risco num extra opcional.

Grafana, Loki e dashboards são diferenciais opcionais pelo §12 e **não foram
feitos**.

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
