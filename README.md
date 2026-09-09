# Munchkin — processamento distribuído de apostas

Serviço em Go que movimenta carteiras de jogadores por HTTP e por SQS com
garantias equivalentes, sob entrega *at-least-once*, com várias instâncias em
execução e falhas entre as etapas do processamento.

As decisões técnicas, as interpretações adotadas e as limitações conhecidas
estão em [`ARCHITECTURE.md`](ARCHITECTURE.md).

---

## Estado atual

> Atualizado a cada etapa. O que não está marcado, não existe ainda.

| Etapa | Entrega | Estado |
|---|---|---|
| 00 | Fundação: módulo, gates dos critérios eliminatórios, CI | ✅ |
| 01 | Composição com Fx, Fiber, erros padronizados, `/health/live` | ⬜ |
| 02 | Keycloak, cache de JWKS, middleware de autenticação | ⬜ |
| 03 | Postgres, ciclo de vida, `/health/ready` | ⬜ |
| 04 | Migrations versionadas e schema com as constraints | ⬜ |
| 05 | `Money` | ⬜ |
| 06 | Agregados de domínio | ⬜ |
| 07 | Abertura de carteira | ⬜ |
| 08 | Operação financeira e idempotência | ⬜ |
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
docker compose up --build      # ambiente completo
# ou, para desenvolver com a aplicação fora do container:
make up                        # sobe só as dependências
make migrate-up                # aplica as migrations
make build && ./bin/api
```

Verificação rápida:

```sh
curl -s localhost:8080/health/live
curl -s localhost:8080/health/ready
```

## Migrations

```sh
make migrate-up       # aplica
make migrate-down     # reverte
make migrate-status   # estado atual
```

Migrations são versionadas em pares `.up.sql`/`.down.sql` em `migrations/`.
Migration já aplicada nunca é editada: corrige-se com uma nova.

## Testes

```sh
make test               # unitários
make test-race          # unitários com detector de corrida
make gates              # gates dos critérios eliminatórios
make lint

make test-integration   # Postgres, Keycloak e LocalStack reais
make test-concurrency   # 50 envios paralelos, disputa 80+80, múltiplos processos
make test-recovery      # interrupção entre commit e remoção, publishers concorrentes
```

Os três últimos exigem Docker em execução e usam `testcontainers`: sobem e
derrubam a infraestrutura sozinhos, sem depender de ambiente pré-montado. Rodam
sob a build tag `integration`, então `go test ./...` não os inclui.

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
| API | 8080 |
| Postgres | 5440 |
| Keycloak | 8180 |
| LocalStack | 4576 |
| Prometheus | 9091 |
| Grafana | 3000 |

Ajustáveis pelo `.env`.
