# ARCHITECTURE

Decisões técnicas do Munchkin, o que foi descartado no caminho, as
interpretações adotadas onde o enunciado admite mais de uma leitura, e o que
não foi concluído.

> **Como ler.** Cada seção registra a decisão **e** a alternativa descartada com
> o motivo. Uma seção que só diz o que foi escolhido registra um fato, não uma
> decisão — e o que interessa aqui é o raciocínio.
>
> A coluna de estado do `README.md` diz o que já está implementado. Uma decisão
> registrada aqui e ainda não implementada está marcada como **(planejado)**.

---

## 1. Forma geral

```
HTTP  ─┐
       ├─→ caso de uso ─→ domínio ─→ repositórios ─→ PostgreSQL
SQS   ─┘        (uma transação: estado, saldo, ledger, inbox, outbox)
                                                          │
                                    worker publisher ←─────┘ (depois do commit)
                                            └─→ SQS
```

HTTP e SQS entram no **mesmo** caso de uso e recebem as mesmas garantias. Isso é
deliberado: se as duas portas tivessem caminhos distintos, a concorrência entre
elas — que o §10 manda validar — dependeria de dois códigos concordarem. Com um
caminho só, a garantia está onde os dois se encontram, no banco.

Camadas:

| Pacote | Responsabilidade | Pode importar |
|---|---|---|
| `internal/domain` | agregados, invariantes, transições, erros de negócio | só a biblioteca padrão |
| `internal/app` | casos de uso; orquestra domínio, repositórios e transação | domínio |
| `internal/adapter` | HTTP, persistência, SQS, IdP | tudo |
| `internal/worker` | processos de fundo | app, adapter |
| `pkg` | utilitários sem regra de negócio | — |

A pureza do domínio não é convenção, é gate: `make gate-domain-pure` roda
`go list -deps ./internal/domain/...` e falha se aparecer `gofiber`, `gorm.io`,
`go.uber.org/fx` ou `aws-sdk-go`.

## 2. Composição com Uber Fx e ciclo de vida

A aplicação é montada por `fx.Module`, com injeção por construtor. Cada módulo
— configuração, logger, banco, IdP, repositórios, casos de uso, servidor HTTP,
workers — declara o que provê e o que consome, e o grafo é resolvido no start.

`fx.Lifecycle` governa recursos com ordem garantida:

- **`OnStart`** valida configuração e dependências antes de aceitar tráfego, e
  falha rápido: chave do IdP indisponível, migration pendente ou banco
  inalcançável impedem a subida em vez de virarem erro no primeiro request.
- **`OnStop`** para de aceitar entrada, conclui o trabalho em andamento dentro
  do prazo e só então fecha as dependências. Fx encerra na ordem inversa da
  inicialização, então o pool do banco fecha depois dos workers que o usam —
  o que evita o erro clássico de derrubar a conexão sob um worker ativo.

O domínio não conhece Fx. Fx monta objetos; ele não aparece em assinatura de
função de negócio.

## 3. Dinheiro

`Money` é um value object imutável com dois campos não exportados: o valor em
**unidades mínimas** (`int64`) e a moeda (ISO 4217). `"25.00"` é `2500`;
`"25.37"` é `2537`. Casas decimais são exatas porque centavos são inteiros.

O parsing é escrito à mão, dígito a dígito. **Não passa por
`strconv.ParseFloat` em momento nenhum**, nem como etapa intermediária. Rejeita
vazio, `NaN`, `Infinity`, notação científica, escala acima de duas casas e
negativo em entrada financeira externa. Não há arredondamento silencioso: uma
entrada inválida é erro, nunca um valor corrigido às escondidas.

Persistência em `amount_minor BIGINT` + `currency CHAR(3)`.

**Limites.** `int64` cobre de `-92.233.720.368.547.758,08` a
`+92.233.720.368.547.758,07`. O complemento de dois é assimétrico, então negar o
menor valor estoura — caso tratado explicitamente. Overflow é verificado em
parsing, soma, subtração e negação, sempre **antes** da operação, comparando
contra o limite, e nunca depois pelo sinal do resultado.

Valores negativos existem em diferenças e cálculos internos; são rejeitados na
entrada externa e impossíveis no saldo, que tem `CHECK (balance_minor >= 0)`.

**Descartado — `shopspring/decimal`:** a escolha óbvia, e o motivo de não a usar
é específico. A API expõe `NewFromFloat` e `InexactFloat64`; uma única chamada
dessas em qualquer ponto viola a proibição de ponto flutuante, e nada na
biblioteca impede que aconteça. Adotá-la exigiria um linter proibindo
construtores da própria dependência — sinal de que a dependência não serve ao
requisito. Com `int64`, a proibição se prova por `grep`, e isso virou gate.

**Descartado — `govalues/decimal`:** tecnicamente boa, mas resolve um problema
que não existe aqui. Precisão arbitrária serve quando a escala varia; a escala
é fixa em duas casas por exigência do contrato, e o teto do `int64` está ordens
de grandeza acima de qualquer saldo plausível.

**Descartado — `NUMERIC` no Postgres:** manteria a precisão no banco, mas a
aritmética continuaria em Go. Move o problema em vez de resolvê-lo, e `BIGINT`
soma e compara mais barato no `UPDATE` do caminho quente.

## 4. Transações e atomicidade

Estado da operação, saldo, lançamento do ledger, registro de inbox e registros
de outbox são confirmados **no mesmo commit**, ou não são confirmados.

A transação é aberta pelo caso de uso e propagada aos repositórios pelo
`context.Context`, por um gerenciador de transação. O repositório não decide se
está numa transação: ele lê do contexto o handle corrente e, na ausência,
usa o pool. Assim a fronteira transacional fica no caso de uso, onde ela é
legível, e não espalhada pelos repositórios.

Sequência canônica, com ordem de aquisição de lock fixa — é a ordem fixa que
impede deadlock:

```
BEGIN
 1. INSERT wager_transactions (PENDING) ON CONFLICT DO NOTHING RETURNING id
      vazio → replay ou conflito de idempotência (ver §6)
 2. SELECT ... FROM wallets WHERE id = $1 FOR UPDATE
 3. reversões: resolve a referência (ver §8)
 4. o domínio decide; rejeição de negócio grava REJECTED e ainda assim commita
 5. UPDATE wallets SET balance_minor = $, version = version + 1
      WHERE id = $ AND version = $      -- afeta exatamente 1 linha
 6. INSERT wallet_ledger_entries        -- LOSS não gera lançamento
 7. UPDATE wager_transactions (PROCESSED)
 8. INSERT outbox_events
COMMIT
```

Uma rejeição de negócio **não** é rollback: o registro `REJECTED`, seu código de
falha e o evento de rejeição precisam sobreviver, senão a operação some e o
provedor reenviaria para sempre. Como a decisão acontece antes de qualquer
escrita de saldo, basta gravar o desfecho e commitar — não é preciso savepoint.

### 4.1 ACID, letra por letra

Não basta afirmar que o sistema é transacional. Cada garantia tem um mecanismo
concreto, e é ele que responde quando alguém pergunta como.

| | Garantia | Como é obtida aqui |
|---|---|---|
| **A** | Atomicidade | Uma transação SQL cobre estado da operação, saldo, lançamento do ledger, registro de inbox e eventos de outbox. Não há segunda fase, nem escrita fora dela. Rejeição de negócio também commita — o registro terminal e seu evento fazem parte do resultado, não são desistência. |
| **C** | Consistência | As invariantes vivem no schema, não no código: `CHECK (balance_minor >= 0)`, unicidade de `(providerId, externalTransactionId)`, unicidade de `(walletId, transactionId)` no ledger, índice parcial permitindo no máximo uma reversão bem-sucedida por referência, e `CHECK` separando operação interna de externa. Um caminho de código com defeito é recusado pelo banco; a garantia não depende de a aplicação estar correta. |
| **I** | Isolamento | `SELECT ... FOR UPDATE` na linha da carteira serializa os escritores daquela carteira. A leitura que embasa a decisão do domínio acontece **depois** de o lock ser adquirido, então ela enxerga o estado já commitado por quem chegou antes. |
| **D** | Durabilidade | O `COMMIT` é a fronteira do que existe. Evento publicado antes do commit é proibido — o worker de outbox só enxerga o que já está em disco. Uma queda entre commit e publicação é recuperável; uma publicação antes do commit não. |

**Nível de isolamento: `READ COMMITTED`, o padrão do Postgres.** A pergunta
natural é por que não `SERIALIZABLE`, e a resposta é que ele não acrescenta
garantia aqui e cobra caro.

O que precisamos evitar é *lost update* no saldo. `READ COMMITTED` sozinho não
evita — mas `READ COMMITTED` com lock explícito de linha evita, porque o segundo
escritor só lê depois que o primeiro commitou. `SERIALIZABLE` daria o mesmo
resultado por outro caminho: detectando o conflito e **abortando** uma das
transações com erro de serialização, que a aplicação teria de reexecutar. Isso
significa laço de retry em todo caminho financeiro, e um cenário de teste cujo
resultado é certo mas cujo caminho varia.

Trocamos detecção-e-repetição por prevenção. Além disso, as invariantes que mais
importam — não negatividade, unicidade, imutabilidade do ledger — são impostas
por constraint, e constraint vale em qualquer nível de isolamento.

**Anomalias que assumimos.** Em `READ COMMITTED`, duas leituras dentro da mesma
transação podem ver estados diferentes. Isso não afeta o caminho financeiro,
onde a única leitura que decide acontece sob lock. Afeta, em tese, a
reconciliação, que percorre muitos lançamentos — por isso ela roda numa visão
consistente dos dados, e não em leituras soltas.

**Uso de GORM.** GORM cobre leitura, listagem, paginação e CRUD simples. O
caminho financeiro — lock, `UPDATE` condicionado, inserção no ledger,
reivindicação da outbox — é **SQL cru**, escrito à mão, porque o enunciado exige
transação, lock e constraint "explícitos e verificáveis" e porque quem revisa
precisa ler o SQL do dinheiro sem decifrar API fluente. `gorm.io/driver/postgres`
roda sobre `pgx`, então a escolha não troca o driver preferido: adiciona uma
camada acima dele, e essa camada fica fora do caminho do dinheiro.

Agregados de domínio e modelos de linha são structs **distintas**. O agregado
tem campos não exportados, como o encapsulamento exige; o modelo de linha tem
campos exportados e tags, como o ORM exige. A tradução é explícita, por
reidratação — e reidratar não reaplica movimentação nem emite evento.

**Descartado — GORM em tudo:** `Updates` com struct ignora campo zero-value por
padrão. Num agregado financeiro isso significa que um saldo que chegou a `0.00`
pode não ser gravado, sem erro. Há como contornar, mas todas as formas dependem
de alguém lembrar; somado a hooks e soft delete, é comportamento implícito
demais para o caminho crítico.

**Descartado — `sqlc`:** daria SQL explícito com tipagem forte, mas adiciona
etapa de geração a um projeto que já tem GORM. Duas formas de falar com o banco
bastam.

## 5. Concorrência e locks

A coordenação é `SELECT ... FROM wallets WHERE id = $1 FOR UPDATE`, dentro da
transação que movimenta. O lock é de **linha**: serializa aquela carteira e só
ela. Carteiras distintas seguem em paralelo, e não existe lock global.

Três camadas que precisam concordar:

1. `FOR UPDATE` serializa os escritores da mesma carteira.
2. `UPDATE ... WHERE id = $1 AND version = $2` afirma que ninguém alterou a
   linha entre a leitura e a escrita. Afetar zero linhas é erro, não retry
   silencioso: com o lock em mãos isso não deveria ocorrer, e o dia em que
   ocorrer é um defeito que queremos ver.
3. `CHECK (balance_minor >= 0)` no schema, como última linha de defesa: mesmo
   que um caminho futuro esqueça o lock, o banco recusa.

Os workers usam `FOR UPDATE SKIP LOCKED` para disputar registros sem se
bloquearem entre si.

**Descartado — mutex em Go.** Foi a primeira ideia considerada e é a mais
diretamente inadequada: um mutex é por processo. Com três instâncias há três
mutexes e nenhuma coordenação — duas apostas concorrentes sobre o mesmo saldo
passariam. Um mutex por carteira dentro do processo reduziria contenção no
banco, mas é otimização somada a um problema de leitura: quem revisa vê "mutex"
no caminho do saldo e passa a procurar a garantia no lugar errado.

**Descartado — controle otimista com CAS e retry limitado.** Legítimo e melhor
sob baixa contenção. O retry, porém, precisa reabrir a transação inteira, porque
a leitura que embasou a decisão do domínio ficou obsoleta — isso espalha a
lógica de concorrência pelo caso de uso e torna o cenário de disputa não
determinístico: o resultado é sempre correto, mas o caminho varia, e caminho
variável é caro de provar.

**Descartado — serialização por partição de fila.** Elegante, e é o que
`MessageGroupId` sugere, mas não cobre a entrada HTTP, que é síncrona e
concorrente com a fila. A garantia precisa morar onde as duas portas se
encontram.

## 6. Idempotência

**Não há cache de idempotência, e a ausência é deliberada.**

A verificação não é "pergunto se já processei, depois insiro". É uma operação só:

```sql
INSERT INTO wager_transactions (...)
ON CONFLICT (provider_id, external_transaction_id) DO NOTHING
RETURNING id;
```

Voltou linha, a operação é nova. Não voltou, é replay — e só então há um
`SELECT`, para devolver o resultado já persistido. O caminho feliz custa uma
escrita, que aconteceria de qualquer forma. **Não existe consulta extra a
otimizar.**

Há ainda um efeito que nenhum cache reproduz: quando muitas requisições
idênticas chegam juntas, as perdedoras bloqueiam **no índice único** até a
vencedora commitar, e então recebem o conflito e leem o resultado dela. O
Postgres resolve a corrida; um cache na frente só teria a chance de errar.

Duas unicidades, ambas por provedor:

- `(provider_id, external_transaction_id)` — a operação financeira. Impede que
  a mesma operação seja reaplicada usando outra chave de idempotência.
- `(provider_id, idempotency_key)` — a chave. Permite distinguir reuso de chave
  com conteúdo diferente.

O corpo é reduzido a um **hash determinístico** dos campos de negócio: JSON
canônico com chaves ordenadas, sem espaços, valores monetários normalizados para
a forma de escala fixa, e SHA-256. A chave de idempotência e os metadados de
transporte ficam **fora** do cálculo — senão o hash mudaria por motivo que não é
de negócio. HTTP e SQS calculam o mesmo hash para o mesmo conteúdo, que é o que
torna as duas portas equivalentes.

Desfechos:

| Situação | Resposta |
|---|---|
| Chave e conteúdo equivalentes | resultado persistido, `idempotentReplay: true` |
| Chave reutilizada com conteúdo diferente | conflito |
| Mesma operação com outra chave | conflito |
| Operação concluída | o saldo devolvido é o **observado no processamento original**, mesmo que a carteira já tenha se movimentado depois |

**Descartado — Redis como cache de resultado:** aceleraria apenas o replay, que
é exceção e não caminho quente. Em troca: mais um serviço, mais um modo de
falha, invalidação e TTL para acertar, e uma política de fail-open a documentar.

**Descartado — LRU em processo:** o mais barato de escrever e o mais caro de
defender. Mesmo restrito a leitura de resultado terminal, coloca a idempotência
parcialmente na memória de um processo, que é justamente o que não pode
acontecer.

**Descartado — tabela de idempotência separada:** adicionaria um `SELECT` que o
`ON CONFLICT` dispensa, e abriria a possibilidade de duas tabelas divergirem
sobre o mesmo fato.

## 7. Referências ainda indisponíveis (planejado)

Uma reversão cuja referência ainda não chegou é persistida como
`PENDING_REFERENCE`, com o evento correspondente, e a resposta indica
processamento pendente. Um worker tenta resolver de novo com backoff
exponencial, e sobrevive a reinício porque o estado está no banco, não na
memória. Esgotado o limite de tentativas ou o TTL, a operação termina como
`REJECTED` com código de referência não encontrada, e o evento de rejeição é
emitido.

Se a referência **existe mas ainda está pendente**, a operação continua
aguardando: o desfecho dela pode mudar. Se a referência **terminou sem
sucesso** — rejeitada ou falha permanente — a reversão é rejeitada de imediato,
com código próprio, porque não há mais o que reverter.

`PENDING_REFERENCE` é o único estado não terminal que chega ao disco.

## 8. Reversões

`REFUND` e `ROLLBACK` exigem `referenceExternalTransactionId`, resolvido por
`(providerId, referenceExternalTransactionId)`. Operação e referência precisam
concordar em provedor, jogador, carteira, moeda e rodada, e o valor da reversão
tem de ser igual ao referenciado — reversão parcial está fora do escopo.

**Interpretação adotada — no máximo uma reversão bem-sucedida por referência.**
O enunciado pede que uma referência não receba duas reversões do mesmo tipo, e
pede também que a combinação de `REFUND` e `ROLLBACK` sobre a mesma aposta não
devolva o mesmo débito duas vezes. A leitura literal da primeira frase
permitiria um `REFUND` e um `ROLLBACK` bem-sucedidos sobre a mesma aposta — o
que devolveria o dinheiro duas vezes e contraria a segunda. Adotamos a regra
mais forte, imposta por índice único parcial:

```sql
CREATE UNIQUE INDEX ON wager_transactions (reference_transaction_id)
  WHERE status = 'PROCESSED' AND kind IN ('REFUND','ROLLBACK');
```

Uma segunda reversão, de qualquer tipo, é rejeitada com código próprio.

**Saldo insuficiente na reversão** tem código de falha **distinto** do usado
para aposta sem saldo. São situações diferentes: a aposta foi recusada por
limite do jogador; a reversão foi recusada porque o dinheiro já saiu da
carteira — e o operador precisa distinguir as duas na auditoria.

## 9. Inbox e outbox

**Outbox.** Os eventos entram na tabela de outbox **dentro** da transação que os
originou. Publicar é trabalho de um worker separado, depois do commit. Nenhum
caminho de código chama SQS antes do `COMMIT`.

O worker reivindica registros com `FOR UPDATE SKIP LOCKED` e lease
(`locked_by`, `locked_until`), o que dá três propriedades ao mesmo tempo:
vários publishers coexistem sem se bloquear, um publisher que morre tem seu
trabalho reassumido quando a lease expira, e nada exige instância única.

Interrupção entre publicar e confirmar resulta em republicação — é
at-least-once por construção. O `eventId` é preservado, e vai como identificador
de deduplicação da mensagem, de modo que o consumidor recebe o mesmo evento uma
vez só.

**Inbox.** Na entrada por SQS, o registro de inbox — identidade da mensagem, do
consumidor e o hash — compartilha a transação das mudanças de domínio, do ledger
e dos eventos. A mensagem só é removida da fila **depois** do commit. Uma
interrupção entre o commit e a remoção causa reentrega, e a reentrega encontra o
registro de inbox já concluído: remove a mensagem sem reprocessar.

Reentrega com hash divergente para a mesma identidade de mensagem é tratada como
mensagem inválida, não como atualização.

## 10. Autenticação

IdP externo **Keycloak**, provisionado por importação de realm versionado no
repositório, com `client_credentials` para comunicação entre serviços.

As chaves públicas são obtidas do JWKS na subida — e a subida falha se não
vierem —, mantidas em memória e atualizadas por ticker. Um `kid` desconhecido
força uma atualização imediata, com limite de frequência, porque é assim que a
rotação de chave se manifesta.

Isso elimina a ida na rede por requisição, **não** a verificação: a assinatura
RS256 é conferida em toda requisição, localmente, junto com emissor, audiência,
expiração e algoritmo. Rejeitar `alg` inesperado é explícito, para fechar a
confusão de algoritmo.

**Emissor fixo.** O Keycloak monta o `iss` do token a partir do endereço por
onde foi alcançado, então o mesmo realm emitiria `iss` diferente conforme o
cliente usasse `127.0.0.1`, `localhost` ou o nome do serviço na rede do
compose — e a validação de emissor passaria a falhar dependendo de quem pediu o
token. O emissor é fixado por configuração no IdP, e a aplicação separa duas
coisas que costumam ser confundidas: o **emissor que ela valida** e a **URL de
onde ela busca** o documento de descoberta. Dentro da rede do compose a segunda
é interna; a primeira continua sendo o endereço público.

A descoberta confere o emissor publicado contra o configurado e recusa a subida
quando divergem. Sem essa checagem, uma URL trocada por engano — outro realm,
outro ambiente — faria o serviço validar felizmente tokens de um IdP que não é
o dele.

Os caminhos públicos são comparados por **igualdade exata**, nunca por prefixo:
prefixo faz uma rota protegida sob `/health/live/` herdar a dispensa de
credencial. A comparação exata está coberta por teste, e o teste foi verificado
trocando a igualdade por prefixo — nessa forma ele fica vermelho e a resposta
vaza o conteúdo protegido.

**Descartado — introspecção remota do token a cada requisição:** estaria sempre
atualizado e permitiria revogação imediata, mas poria o IdP no caminho crítico
de toda operação financeira, transformando indisponibilidade do Keycloak em
indisponibilidade do serviço. O preço da escolha é que a revogação só vale a
partir da expiração — está na lista de limitações.

**Descartado — Zitadel:** o enunciado recomenda Keycloak nominalmente, e
divergir custaria justificativa que não seria técnica. O provisionamento do
Keycloak também é mais simples de auditar: um JSON de realm versionado contra
uma sequência de chamadas a API de management.

## 11. Autorização

A identidade autenticada **determina** o provedor. O `providerId` usado em
qualquer decisão vem do token; se o corpo da requisição discordar, a requisição
é recusada. O servidor nunca confia no corpo para decidir de quem é a operação.

| Escopo | Permite |
|---|---|
| `wagering:write` | enviar operação do próprio provedor |
| `wagering:read` | consultar transações do próprio provedor |
| `wallets:admin` | abertura interna de carteira |

Consultas são filtradas por provedor, inclusive em replay: um provedor não
descobre a existência de transação de outro. Operação de carteira é restrita ao
serviço interno.

**Autenticação antes de roteamento.** Uma rota inexistente responde 401 a quem
não se identificou, e 404 a quem se identificou. É deliberado: 404 contaria a um
chamador anônimo quais caminhos existem, e o mapa de uma API financeira não é
informação pública. O custo é uma resposta menos "correta" em HTTP; o ganho é
não entregar reconhecimento de superfície de graça.

**Autenticado não é autorizado.** A verificação de escopo é um passo separado da
verificação de credencial, e devolve 403, não 401. Misturar as duas é como se
acaba concedendo a um cliente válido uma operação que ele não deveria alcançar.
O realm traz um cliente sem escopo algum, cuja única função é provar essa
distinção em teste.

`OPENING` é reservado à abertura interna e é recusado quando chega por HTTP ou
por SQS.

## 12. Shutdown

Em `SIGTERM`, na ordem: o servidor HTTP para de aceitar conexões novas e drena
as em andamento; o consumidor SQS para de buscar trabalho e conclui o que já
tem — ou libera a visibilidade da mensagem, para reentrega segura, se não couber
no prazo; os workers de fundo observam o cancelamento do contexto e encerram; só
então as dependências fecham.

Nenhum worker é interrompido no meio de uma transação sem que ela seja desfeita:
o commit é atômico, então ou a operação inteira valeu, ou nenhuma parte dela valeu.

## 12.1 Resiliência a pânico

Um pânico não pode derrubar o serviço, e há dois casos com tratamentos
diferentes — a confusão entre eles é uma fonte clássica de queda em produção.

**No tratador HTTP**, o middleware de recuperação converte o pânico em resposta
500 com o corpo padrão. A mensagem do pânico e a pilha vão para o log e nunca
para o cliente: elas revelam caminho de arquivo, nome de função e às vezes valor
de variável. A requisição afetada falha; o processo segue atendendo.

**Em goroutine**, o recover de quem iniciou **não alcança** o pânico — ele
derruba o processo inteiro. Como todo worker é goroutine, cada um é iniciado por
um utilitário que instala o próprio recover, registra a falha com nome da tarefa
e pilha, e mantém o laço vivo com um intervalo de espera entre iterações. O
intervalo existe para que uma falha determinística não vire laço quente
consumindo CPU e enchendo o log.

A distinção entre os dois casos está coberta por teste: um tratador que entra em
pânico devolve 500 sem vazar a pilha e o serviço continua respondendo; uma
goroutine que entra em pânico é registrada e o laço prossegue.

## 13. Observabilidade (planejado)

Logs estruturados em JSON no stdout, carregando os identificadores disponíveis —
correlação, mensagem, transação, carteira, provedor. **Nunca** credencial, dado
sensível ou payload financeiro completo: log carrega identificador, não conteúdo.

Métricas expostas para coleta: resultados por status, duplicatas, retentativas,
DLQ, conflitos de concorrência, atraso da outbox, latência de processamento e
divergências de reconciliação.

Health checks separam vivacidade do processo de prontidão das dependências.

## 14. Interpretações adotadas

Onde o enunciado admite mais de uma leitura, a leitura escolhida e o motivo:

1. **Uma reversão por referência**, não uma por tipo. Detalhado em §8.
2. **Processamento síncrono em commit único.** O enunciado permite concluir de
   forma síncrona operações sem dependências, e a resposta de exemplo do
   endpoint traz o estado processado junto com o saldo — o que só existe se a
   operação concluiu antes de responder. `PENDING` portanto nunca é confirmado
   no caminho feliz, e a exigência de retomada durável se aplica a
   `PENDING_REFERENCE`, que tem worker próprio.
   *Descartado — aceite assíncrono com resposta 202:* atacaria de frente a
   exigência de retomada, mas obrigaria o provedor a fazer polling para saber se
   a aposta valeu, mudando um contrato que o enunciado já define.
3. **Rejeição de negócio commita.** Uma operação recusada por regra de negócio
   grava seu registro terminal e seu evento; não é rollback. Sem isso o provedor
   reenviaria indefinidamente uma operação que já tem desfecho.
4. **`LOSS` não gera lançamento nem altera versão da carteira**, mas gera o
   evento de conclusão — é operação processada com movimentação nula.
5. **Abertura com saldo zero não cria `OPENING`, ledger nem eventos
   financeiros.** Só a carteira, em versão inicial.

## 15. Limitações conhecidas

- **Escala monetária fixa em duas casas.** Não atende moeda de três casas
  decimais nem de zero casas. O tipo carrega a moeda e recusa operação entre
  moedas distintas, mas a escala é uma só.
- **Revogação de token não é imediata.** Consequência de validar por JWKS em
  cache em vez de introspecção remota; um token revogado continua válido até
  expirar. Trade-off explicado em §10.
- **A transação segura o lock da carteira durante a decisão do domínio.** Sob
  contenção alta na mesma carteira a latência sobe — comportamento correto para
  dinheiro, mas exige manter o bloco transacional curto e sem I/O externo.
- **Fiber roda sobre `fasthttp`**, que não é `net/http`. O `context.Context` da
  aplicação é carregado por `c.UserContext()`, e a semântica de cancelamento
  difere da biblioteca padrão. Um gate proíbe o uso do contexto errado nos
  handlers, mas a diferença existe e é relevante para instrumentação futura.
- **Reversão parcial não é suportada**, conforme o escopo definido.

## 16. Trabalho não concluído

Esta seção é mantida honesta ao longo do desenvolvimento. A coluna de estado do
`README.md` é a fonte precisa; aqui ficam as pendências que merecem comentário.

- As etapas 01 a 16 da tabela do `README.md` ainda não foram implementadas.
- Tracing distribuído e testes de carga são diferenciais opcionais e só serão
  considerados depois de o núcleo estar completo e verificado.
