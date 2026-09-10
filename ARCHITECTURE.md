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
- **`OnStop`** para de aceitar entrada, encerra o trabalho em andamento — o HTTP
  drenando as requisições em curso, o consumidor SQS devolvendo à fila o que não
  vai tratar (§12) — e só então fecha as dependências. Fx encerra na ordem inversa da
  inicialização, então o pool do banco fecha depois dos workers que o usam —
  o que evita o erro clássico de derrubar a conexão sob um worker ativo.

O domínio não conhece Fx. Fx monta objetos; ele não aparece em assinatura de
função de negócio.

## 3. Dinheiro

`Money` é um value object imutável com dois campos não exportados: o valor em
**unidades mínimas** (`int64`) e a moeda (ISO 4217). `"25.00"` é `2500`;
`"25.37"` é `2537`. Casas decimais são exatas porque centavos são inteiros.

O parsing é escrito à mão, dígito a dígito. **Não passa por
`strconv.ParseFloat` em momento nenhum**, nem como etapa intermediária. Não há
arredondamento silencioso: uma entrada inválida é erro, nunca um valor corrigido
às escondidas.

Persistência em `amount_minor BIGINT` + `currency CHAR(3)`.

### Formato aceito: uma grafia por valor

O contrato é exato — sinal opcional, parte inteira sem zeros à esquerda, ponto,
**exatamente** duas casas:

| Aceito | Recusado | Por quê |
|---|---|---|
| `"25.00"` | `"25"`, `"25.0"`, `"25.000"` | escala tem de ser exatamente 2 |
| `"0.00"` | `".00"`, `"-0.00"` | parte inteira obrigatória; zero não tem sinal |
| `"-60.00"` | `"+25.00"`, `" 25.00"` | sinal positivo e espaço são grafias alternativas |
| `"25.37"` | `"025.00"` | zero à esquerda é grafia alternativa |
| | `"25,00"`, `"1e2"`, `"NaN"`, `"Infinity"`, `"0x19"`, `"２５.００"` | não é o formato |

O §6.1 do enunciado oferece duas saídas: aceitar formas equivalentes e
**documentar a normalização anterior ao hash de idempotência**, ou não aceitá-las.
Escolhemos a segunda, e a razão é a idempotência: se `"25.00"` e `"025.00"` forem
ambos aceitos, a mesma operação enviada com grafias diferentes gera hashes
diferentes e vira duas operações. Recusando as alternativas, **não existe
normalização a documentar** — uma peça a menos no caminho da idempotência, e uma
peça a menos que pode divergir entre a entrada HTTP e a entrada por fila.

O custo é real: um provedor que envie `"25.0"` recebe recusa. A mensagem de erro
diz exatamente qual é o problema, e não uma falha genérica de parsing.

Que o formato aceito seja também o formato produzido é propriedade verificada por
fuzzing: toda entrada aceita, ao ser reformatada, tem de voltar a ser aceita e
produzir o mesmo valor.

### Limites

`int64` em unidades mínimas cobre de `-92.233.720.368.547.758,07` a
`+92.233.720.368.547.758,07`.

A faixa é **simétrica por decisão**, não por acaso. O menor `int64` é recusado na
construção porque sua negação não seria representável: aceitá-lo criaria um valor
legítimo que faz `Neg` estourar mais adiante, longe de onde foi criado. Recusar
na entrada elimina o caso de borda em vez de tratá-lo em cada operação.

Overflow é verificado em parsing, soma e subtração **antes** da operação,
comparando contra o limite — nunca depois, inspecionando o sinal do resultado.
Em Go, estouro de inteiro com sinal não gera pânico: ele dá a volta em silêncio,
e num sistema financeiro isso é um saldo absurdo sem rastro.

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

Sequência canônica. A ordem de aquisição de lock é fixa, e é ela que impede
deadlock: toda operação toma primeiro a linha da carteira, depois a da transação.

**A carteira é travada antes da reivindicação de idempotência**, e não o
contrário. A operação referencia a carteira por chave estrangeira composta
`(carteira, moeda)`, então uma operação para carteira inexistente — ou em moeda
divergente — **não pode sequer ser inserida**: precisa ser recusada antes da
tentativa. O custo é que um replay também toma o lock da carteira; a alternativa
seria uma consulta extra no caminho de toda operação nova, que é o caminho
quente.

```
BEGIN
 1. SELECT ... FROM wallets WHERE id = $1 FOR UPDATE
      inexistente → recusa de alvo, nada gravado (ver §14.6)
 2. valida o alvo: jogador e moeda
      divergente → recusa de alvo, nada gravado
 3. INSERT wager_transactions (PENDING) ON CONFLICT DO NOTHING
      não inseriu → replay ou conflito de idempotência (ver §6)
 4. reversões: resolve a referência (ver §8)
 5. o domínio decide; recusa de negócio grava REJECTED e ainda assim commita
 6. UPDATE wallets SET balance_minor = $, version = version + 1
      WHERE id = $ AND version = $      -- afeta exatamente 1 linha
 7. INSERT wallet_ledger_entries        -- LOSS não gera lançamento
    + INSERT ledger_postings (par)      -- projeção; ver §4.3
 8. UPDATE wager_transactions (PROCESSED | REJECTED)
 9. INSERT outbox_events
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

**Dois papéis no banco, e a razão não é cerimônia.** A aplicação conecta como
`munchkin_app`, que **não é dono** do schema; as migrations rodam como o dono.
Em PostgreSQL, o dono de uma tabela tem privilégio por *ownership*, não por
concessão — revogar `UPDATE` e `DELETE` do dono não adianta, porque ele pode se
reconceder a qualquer momento. A imutabilidade do ledger imposta pelo banco só
existe se quem escreve não for o dono. Privilégios padrão no schema concedem ao
papel da aplicação o acesso às tabelas que as migrations criarem, para que
nenhuma migration precise lembrar de conceder — a que esquecesse só falharia em
execução.

**Fronteira transacional no caso de uso.** A transação é aberta pelo caso de uso
e propagada aos repositórios pelo `context.Context`. O repositório pede o handle
corrente e recebe a transação, se houver uma, ou o pool. Assim ele não sabe nem
precisa saber se está dentro de uma transação, e a fronteira fica legível num
lugar só. Chamada aninhada reaproveita a transação corrente em vez de abrir uma
segunda: abrir outra quebraria a atomicidade em silêncio.

Um pânico dentro do bloco desfaz a transação **antes** de subir. Sem isso a
conexão volta ao pool com transação aberta, e a próxima operação a pegá-la
herda o estado sujo. O pânico é repropagado — engoli-lo converteria um defeito
de programação em erro de negócio silencioso.

**O padrão do GORM de embrulhar cada escrita solta numa transação própria está
desligado.** Não é otimização: é manter visível quem abre e quem fecha
transação. Com ele ligado, uma escrita fora do bloco transacional commitaria
sozinha sem ninguém perceber.

**O log do banco não carrega valores.** O registro de consulta do GORM interpola
os parâmetros dentro do SQL por padrão, o que poria valor monetário,
identificador de jogador e chave de idempotência em texto claro no log. A
interpolação está desligada: o log mostra a consulta com marcadores de posição.

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

### 4.2 Migrations

Pares `.up.sql`/`.down.sql` versionados, **embarcados no binário** por `go:embed`.
Embarcar em vez de montar um diretório significa que a imagem é autossuficiente:
não há como o container subir com uma versão do código e outra do schema, que é
uma das formas mais desagradáveis de um deploy dar errado.

O executor é um subcomando do mesmo binário, mas roda como um **serviço
separado** no ambiente, com as credenciais do dono do schema. A aplicação nunca
as recebe — é o que mantém efetivo o `REVOKE` sobre o ledger.

Migration já aplicada nunca é editada; corrige-se com uma nova. Se uma migração
for interrompida no meio, o executor marca o banco como sujo e **recusa operar**
até intervenção explícita: aplicar por cima de um estado que ele não sabe
classificar é como se produz um schema meio aplicado que ninguém reproduz.

A reversão completa exige confirmação por variável de ambiente. Reverter tudo
apaga os dados, e não é a operação que alguém quer por ter digitado depressa.

### 4.3 Partidas dobradas

Diferencial **opcional** do §6.4, e por isso ele entra sem tocar no que o
enunciado especifica: `wallet_ledger_entries` fica exatamente como está, e as
partidas vivem em `ledger_postings`, ao lado.

**A partida é projeção do lançamento, não origem independente.** Cada lançamento
vira um par que soma zero — a carteira de um lado, a **casa** do outro — escrito
na mesma instrução, dentro da mesma transação. A função que cria partidas sempre
cria as duas; não existe uma que devolva metade de um par, e por isso não existe
caminho que escreva metade.

A conta da casa é identificada pela **moeda**: dinheiro de moedas diferentes não
se soma, e um balancete que misturasse duas não seria balancete.

**O valor da partida tem sinal.** A invariante desta tabela é "a soma dá zero", e
uma soma sobre coluna com sinal é a expressão direta disso — no banco vira
`SUM(amount_minor) = 0`, que se lê igual à regra.

**Quem confere é o banco, no commit.** Um `CONSTRAINT TRIGGER` diferido soma as
partidas da transação e recusa o commit se sobrar qualquer coisa, faltar metade
ou houver mais de uma moeda. Diferido porque as duas metades podem chegar em
instruções diferentes: um gatilho imediato dispara no fim da *instrução* e
recusaria a metade que chega sozinha, mesmo que a transação fechasse depois.
Como a aplicação escreve o par numa instrução só, ela não precisa do diferimento
— quem precisa é o caminho que alguém escrever amanhã.

A tabela é append-only nas mesmas duas camadas do ledger: gatilho e privilégio
revogado do papel da aplicação.

`GET /ledger/trial-balance` expõe o balancete, com escopo `wallets:admin`. É a
conferência que a reconciliação por carteira não faz: ela olha uma carteira de
cada vez, e o balancete olha a plataforma inteira.

### 4.4 Máquina de estados da operação

Declarada em dado, não em condicionais espalhadas —
`internal/domain/wagering/kind.go`. Estado ausente do mapa, ou com conjunto
vazio, é terminal.

| Estado | Significado | Pode ir para |
|---|---|---|
| `PENDING` | Registro aceito, processamento não concluído | `PENDING_REFERENCE`, `PROCESSED`, `REJECTED`, `FAILED` |
| `PENDING_REFERENCE` | Depende de uma referência que ainda não chegou | `PROCESSED`, `REJECTED`, `FAILED` |
| `PROCESSED` | Concluída com sucesso | — terminal |
| `REJECTED` | Recusada por regra de negócio | — terminal |
| `FAILED` | Falha permanente registrada para auditoria | — terminal |

Terminal não transiciona nem para si mesmo: um replay **consulta** o resultado
persistido, nunca reaplica. A validação é do domínio, e uma transição não
declarada é erro tipado (`ErrInvalidTransition`), não um `if` esquecido.

**`PENDING` nunca é confirmado no caminho feliz.** O enunciado permite concluir
de forma síncrona operações sem dependências, e é o que fazemos: o `INSERT` em
`PENDING` e o `UPDATE` para o estado final acontecem na mesma transação. Só
`PENDING_REFERENCE` chega ao disco em estado não terminal — e é por isso que ele
é o único que precisa de worker de retomada.

**Transitória ou permanente: quem decide, e o que muda.** A mesma pergunta
aparece em três lugares, e a resposta muda em cada um:

| Onde | Transitória | Permanente |
|---|---|---|
| HTTP | `503 SERVICE_UNAVAILABLE`, retentável. Nada persiste | `500 INTERNAL_ERROR`, com `correlationId` |
| Mensagem na fila | Mensagem mantida; volta a ficar visível e é retentada; o redrive a leva à DLQ na quinta entrega | DLQ **imediatamente**, com o motivo em atributo — envelope inválido, campo irrecuperável, reentrega divergente |
| Pendência de referência | Reagendada com backoff exponencial, dentro do orçamento de tentativas | Esgotado o orçamento vira **`FAILED`** com `INTERNAL_ERROR` |

A classificação vem do **SQLSTATE**, por classe e não por código: `08` (conexão),
`57` (operador intervindo) e `53300` (conexões esgotadas) são transitórios; o
resto não é. Classificar por classe evita a lista de códigos que envelhece.

O `FAILED` da terceira linha é a única forma de a operação chegar a esse estado.
Ele existe porque, sem ele, uma pendência que falhasse por algo que não sara
sozinho giraria para sempre: a transação seria desfeita, o contador de tentativas
não avançaria — ele vive dentro dela — e a linha voltaria em toda rodada. Duas
cautelas, porque marcar dinheiro como terminal não se desfaz: indisponibilidade
transitória **não conta**, e uma falha só não basta — o orçamento é o mesmo da
busca por referência, então um erro mal classificado precisa se repetir, com
backoff, antes de causar dano.

Não há evento para `FAILED`: o §11 fixa quatro, e nenhum descreve isto. É
registro para o operador, e o provedor continua podendo consultar a operação.

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
canônico com chaves ordenadas e SHA-256. A ordenação vem do `encoding/json`, que
serializa mapas com as chaves ordenadas — propriedade documentada da biblioteca
padrão. Duas requisições com os mesmos campos em ordens diferentes no corpo
produzem o mesmo hash.

O conjunto de campos é **fixo** e é contrato:

```
externalTransactionId, gameId, kind, money.amount, money.currency,
playerId, providerId, referenceExternalTransactionId, roundId, walletId
```

A referência aparece sempre, vazia quando não se aplica. Incluí-la
condicionalmente faria o mesmo conteúdo produzir hashes diferentes conforme o
campo estivesse presente ou ausente no corpo — e o cliente não controla isso de
forma confiável.

O valor entra na forma canônica produzida pelo tipo, não no texto cru recebido.
Como o tipo aceita uma grafia por valor (§3), a normalização é a própria recusa
do que não é canônico: não existem duas grafias que cheguem ao cálculo.

**Fora do hash, de propósito:** a chave de idempotência, o instante de
recebimento, o estado, o identificador interno, o número de tentativas e o
identificador de correlação. Todos mudam entre um envio e o reenvio da **mesma**
operação, e incluí-los faria todo reenvio parecer conteúdo diferente.

HTTP e SQS constroem o mesmo conjunto de campos, então produzem o mesmo hash
para o mesmo negócio — é o que torna as duas portas equivalentes.

Desfechos:

| Situação | Resposta |
|---|---|
| Chave e conteúdo equivalentes | resultado persistido, `idempotentReplay: true` |
| Chave reutilizada com conteúdo diferente | conflito |
| Mesma operação com outra chave | conflito |
| Operação concluída | o saldo devolvido é o **observado no processamento original**, mesmo que a carteira já tenha se movimentado depois |

**Nem toda recusa ancora a chave, e a distinção é deliberada.** Recusa por alvo
inválido — carteira inexistente, carteira de outro jogador, moeda divergente,
valor incompatível com o tipo — acontece **antes** do `INSERT` e não grava linha
nenhuma. Sem linha não há o que reproduzir: a mesma chave volta a ser avaliada
do zero, e pode terminar diferente se o mundo tiver mudado — uma chave recusada
com `WALLET_NOT_FOUND` processa normalmente se a carteira passar a existir.

A recusa por **saldo**, ao contrário, é gravada: ela decidiu sobre dinheiro
depois de tomar o lock da carteira. O replay dela devolve o desfecho persistido
mesmo que a carteira já tenha sido creditada — reavaliar transformaria a mesma
chave em duas decisões financeiras diferentes.

O critério é esse: recusa que não teve efeito nenhum não é fato a preservar;
recusa que olhou para o saldo é. A ausência de `transactionId` na resposta é o
que distingue as duas para quem integra.

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

## 7. Referências ainda indisponíveis

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

Uma segunda reversão, de qualquer tipo, é rejeitada com
`REFERENCE_ALREADY_REVERSED`.

Quem **responde** é uma consulta, feita com a carteira já travada; quem
**garante** é o índice. A distinção importa: o índice sozinho abortaria a
transação e devolveria erro interno, sem registro nem evento. Com a carteira
travada não há corrida entre a consulta e a gravação — toda reversão da mesma
referência disputa a mesma carteira, e sob `READ COMMITTED` quem adquire o lock
depois lê o commit de quem passou antes.

Isso **não** contradiz a regra de não consultar antes de inserir, aplicada à
idempotência e à abertura de carteira. Lá a corrida é entre duas inserções da
mesma chave, e nenhuma consulta prévia fecha a janela entre o `SELECT` e o
`INSERT`. Aqui a condição está em outra linha, e existe um lock cobrindo a
janela. O critério é esse: consultar antes só é correto quando algo serializa os
concorrentes.

**Saldo insuficiente na reversão** tem código de falha **distinto** do usado
para aposta sem saldo. São situações diferentes: a aposta foi recusada por
limite do jogador; a reversão foi recusada porque o dinheiro já saiu da
carteira — e o operador precisa distinguir as duas na auditoria.

### 8.1 Catálogo de códigos de falha

Toda recusa devolve um código estável. É contrato: uma vez publicado, o código
não muda de significado, porque o provedor decide o que fazer com base nele e
alterar o sentido quebraria integração alheia em silêncio.

A coluna **corrigível** é o que o §7 pede para distinguir: entrada que o provedor
pode ajustar e reenviar, versus resultado definitivo.

| Código | Significado | Corrigível pelo provedor? |
|---|---|---|
| `INSUFFICIENT_FUNDS` | A aposta excede o saldo do jogador | Não — depende do jogador depositar |
| `REVERSAL_INSUFFICIENT_FUNDS` | A reversão precisaria debitar mais que o saldo disponível | Não — o dinheiro já saiu da carteira |
| `REFERENCE_NOT_FOUND` | A referência não chegou dentro do prazo de espera | Sim — reenviar a operação referenciada e depois a reversão |
| `REFERENCE_NOT_PROCESSED` | A referência existe mas terminou sem sucesso | Não — não há o que reverter |
| `REFERENCE_ALREADY_REVERSED` | A referência já foi revertida | Não — reverter de novo devolveria o mesmo débito duas vezes |
| `REFERENCE_MISMATCH` | A referência diverge em provedor, jogador, carteira, moeda ou rodada | Sim — corrigir o identificador referenciado |
| `AMOUNT_MISMATCH` | O valor da reversão difere do referenciado | Sim — reversão parcial está fora do escopo; enviar o valor integral |
| `CURRENCY_MISMATCH` | A moeda diverge da carteira | Sim — corrigir a moeda |
| `WALLET_NOT_FOUND` | A carteira indicada não existe | Sim — corrigir o identificador |
| `WALLET_PLAYER_MISMATCH` | A carteira não pertence ao jogador informado | Sim — corrigir jogador ou carteira |
| `INVALID_AMOUNT_FOR_KIND` | O valor não é o exigido pelo tipo (`LOSS` exige zero; os demais, positivo) | Sim — corrigir o valor |
| `BALANCE_LIMIT_EXCEEDED` | O crédito levaria o saldo além do maior valor representável (±92.233.720.368.547.758,07) | Não com o mesmo valor — o resultado não cabe, e repetir dá a mesma recusa |
| `INTERNAL_ERROR` | Falha permanente de infraestrutura, registrada para auditoria em `FAILED`. Único caminho: pendência de referência que esgota o orçamento falhando por algo que não é transitório (§4.4) | Não — o registro existe para o operador investigar |

Os dois primeiros são deliberadamente distintos, como o §7 exige. Para quem
audita, "o jogador não tinha saldo para apostar" e "o dinheiro já saiu da
carteira, não dá para estornar" são situações diferentes, e um código único
apagaria a diferença.

## 9. Inbox e outbox

**Outbox — gravação.** Os eventos entram na tabela de outbox **dentro** da
transação que os originou. Publicar é trabalho de um worker separado, depois do
commit.

Que nenhum caminho de código publique antes do `COMMIT` não depende de
disciplina: os pacotes que abrem a transação financeira não enxergam o
publicador, e um gate de CI recusa o build se passarem a enxergar. Gravar na
outbox continua permitido — é o que precisa acontecer dentro da transação.

**Outbox — publicação.** O worker reivindica registros com
`FOR UPDATE SKIP LOCKED` e lease
(`locked_by`, `locked_until`), o que dá três propriedades ao mesmo tempo:
vários publishers coexistem sem se bloquear, um publisher que morre tem seu
trabalho reassumido quando a lease expira, e nada exige instância única.

Interrupção entre publicar e confirmar resulta em republicação — é
at-least-once por construção. O `eventId` é preservado, e vai como identificador
de deduplicação da mensagem, de modo que o consumidor recebe o mesmo evento uma
vez só.

Os dois mecanismos da reivindicação fazem coisas diferentes, e a distinção foi
verificada por mutação: **o filtro de lease é o que garante** que ninguém
publique o que já é de outro — removê-lo faz dois publicadores enviarem tudo em
duplicata; **o `SKIP LOCKED` é desempenho**, faz quem perde a disputa seguir
adiante em vez de esperar o lock, e removê-lo mantém o resultado correto, só
mais lento.

Todos os prazos da reivindicação — validade do lease e próxima tentativa — são
calculados com o relógio do **banco**, nunca com o do processo. É o único
relógio que todas as instâncias compartilham: com o relógio local, uma máquina
adiantada seguraria registros além do combinado e uma atrasada perderia o
próprio trabalho para as outras.

Publicar é uma chamada de rede, e ela acontece **fora** de qualquer transação
SQL. Reivindicar, publicar e confirmar são três operações independentes, cada
uma atômica em si. Segurar uma transação aberta durante a chamada de rede
prenderia conexão do pool pelo tempo do transporte e transformaria uma
lentidão do SQS em indisponibilidade do banco.

**Fila de saída.** `wager-events.fifo`, com `MessageGroupId = aggregateId` e
`MessageDeduplicationId = eventId`. O grupo é a carteira: eventos de uma mesma
carteira chegam em ordem, e carteiras distintas não se serializam entre si.
`ContentBasedDeduplication` fica desligado de propósito — ligado, o SQS
deduplicaria por hash do corpo, e dois eventos legítimos de conteúdo idêntico
dentro da janela de cinco minutos seriam engolidos.

**Inbox.** Na entrada por SQS, o registro de inbox — identidade da mensagem, do
consumidor e o hash — compartilha a transação das mudanças de domínio, do ledger
e dos eventos. A mensagem só é removida da fila **depois** do commit. Uma
interrupção entre o commit e a remoção causa reentrega, e a reentrega encontra o
registro de inbox já concluído: remove a mensagem sem reprocessar.

Reentrega com hash divergente para a mesma identidade de mensagem é tratada como
mensagem inválida, não como atualização — aceitá-la seria permitir que alguém
reescrevesse o passado reusando um identificador já gasto.

O destino da mensagem tem três casos. Processada ou já concluída: removida.
Recusa de negócio: **também** removida, porque é desfecho e não falha — está
persistida, com código e evento, e retentar produziria a mesma recusa para
sempre. Falha transitória: mantida, volta a ficar visível, e o redrive a leva à
DLQ depois de cinco entregas. Erro permanente — envelope inválido, campo
irrecuperável, reentrega divergente — vai à DLQ **imediatamente**, porque o
resultado já é conhecido na primeira tentativa e quatro reentregas só atrasariam
o diagnóstico.

**A entrada por SQS não tem token.** No HTTP o `providerId` vem do token e nunca
do corpo; na mensagem não há alternativa, e ele vem do corpo. A fronteira de
confiança passa a ser a **política de acesso da fila**: quem consegue publicar em
`wager-transactions.fifo` é considerado autorizado. É a limitação real do
desenho, e está declarada aqui de propósito. O que a aplicação garante do seu
lado é que `OPENING` continua recusado pela mesma validação do HTTP — um produtor
que pudesse enviá-la creditaria carteira sem passar pela abertura.

**Credencial e política, que são coisas diferentes.** O §2 do enunciado pede as
duas, e elas respondem a perguntas distintas: a credencial diz *quem é*, a
política diz *o que essa identidade pode fazer naquela fila*.

- **Credencial.** O cliente monta credenciais estáticas explícitas em vez de usar
  a cadeia padrão do SDK. A cadeia procura em variáveis, arquivo de perfil,
  metadados de instância e mais alguns lugares, na ordem — e o que ela encontra
  depende de onde o processo está rodando. Numa aplicação que move dinheiro,
  "depende do ambiente" é o tipo de resposta que só se descobre errada em
  produção. As três variáveis são obrigatórias no boot: sem elas a aplicação
  **não sobe**, em vez de subir e falhar na primeira mensagem.
- **Política.** Cada fila é criada com uma `Policy` que permite exatamente as
  ações de quem a usa, e nada além: a de entrada é consumida (`ReceiveMessage`,
  `DeleteMessage`, `ChangeMessageVisibility`), a de saída é apenas publicada
  (`SendMessage`), e a DLQ recebe e é lida para diagnóstico. O princípio é o de
  menor privilégio expresso no broker, e não na disciplina de quem configura.

**O LocalStack não impõe a política — e isso fica dito.** Ele aceita qualquer
credencial não vazia e não avalia a `Policy`. Localmente, portanto, a política é
declaração de intenção: ela descreve o que valeria num SQS real e viaja com o
repositório em vez de existir só na cabeça de alguém. Trocar o LocalStack por
AWS não muda uma linha do provisionamento — muda apenas quem passa a fazer
valer o que já está escrito.

### 9.1 Eventos gravados na outbox

Quatro eventos, cada um com tipo e versão declarados pelo próprio conteúdo — o
chamador não escolhe nenhum dos dois, então não existe a possibilidade de
publicar um evento com o tipo de outro.

| Evento | Gatilho |
|---|---|
| `WagerTransactionProcessed` | Conclusão bem-sucedida, **inclusive `LOSS`** — que conclui sem movimentar a carteira |
| `WagerTransactionRejected` | Recusa definitiva por regra de negócio, com o código de falha |
| `WagerTransactionPendingReference` | Registro da espera por uma referência ainda indisponível |
| `WalletBalanceChanged` | Alteração **efetiva** do saldo — `LOSS` não produz este evento |

**Envelope**, comum aos quatro:

```json
{
  "eventId":       "0192f2a1-...",
  "eventType":     "WalletBalanceChanged",
  "aggregateType": "wallet",
  "aggregateId":   "0192f291-...",
  "correlationId": "01a083fe-...",
  "causationId":   null,
  "occurredAt":    "2026-09-09T12:00:00.000Z",
  "version":       1,
  "data":          { }
}
```

**A carteira é o agregado dos quatro**, inclusive dos três que falam de uma
transação. Não é só mecânica: uma aposta recusada por saldo é um fato sobre
aquela carteira, e quem acompanha uma carteira quer os quatro na ordem em que
aconteceram. De quebra, o `aggregateId` já é a chave de particionamento que a
fila FIFO precisa — eventos da mesma carteira em ordem, carteiras distintas em
paralelo.

**Conteúdo de `WalletBalanceChanged`**, com os campos que o §11 nomeia:

```json
{
  "walletId":      "0192f291-...",
  "transactionId": "0192f298-...",
  "direction":     "DEBIT",
  "money":         { "amount": "80.00", "currency": "BRL" },
  "balanceBefore": { "amount": "100.00", "currency": "BRL" },
  "balanceAfter":  { "amount": "20.00", "currency": "BRL" },
  "walletVersion": 2,
  "changedAt":     "2026-09-09T12:00:00.000Z"
}
```

Instantes em RFC 3339 UTC; valores monetários em string decimal, nunca número
JSON — número vira ponto flutuante na maioria dos leitores, e o valor perderia
precisão antes de o consumidor sequer olhá-lo.

O `eventId` é estável e **sobrevive a republicação**: uma queda entre publicar e
confirmar faz o evento sair de novo com o mesmo identificador, e é por ele que o
consumidor deduplica. Sem essa estabilidade, *at-least-once* viraria duplicata
de verdade.

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
as em andamento; o consumidor SQS para de buscar trabalho e **devolve à fila**
o que não vai tratar; os workers de fundo observam o cancelamento do contexto e
encerram; só então as dependências fecham.

Nenhum worker é interrompido no meio de uma transação sem que ela seja desfeita:
o commit é atômico, então ou a operação inteira valeu, ou nenhuma parte dela valeu.

**O consumidor não tenta concluir a mensagem em voo.** O §10 admite duas saídas
— concluir dentro do prazo ou liberar a visibilidade — e a segunda é a escolhida.
Concluir exigiria manter viva uma transação financeira enquanto o processo morre,
e um encerramento que espera pelo banco é um encerramento que pode não acontecer.

Então o cancelamento chega ao tratamento em curso, a transação é desfeita, a
mensagem **não** é removida — e o consumidor chama `ChangeMessageVisibility` com
zero nela e em todas as do lote que ainda não começaram. Elas voltam a ficar
visíveis **na hora**, e outra instância as pega.

A chamada de devolução roda num contexto descolado do cancelamento
(`context.WithoutCancel`, com prazo próprio de 3s): feita com o contexto que
chegou, ela falharia antes de sair do processo. Se ainda assim falhar, nada se
perde — a mensagem continua na fila e volta quando o visibility timeout de 30s
expirar, que era exatamente o comportamento anterior a esta devolução.

*Descartado — contexto de graça para a mensagem terminar:* deixaria o desligamento
refém do banco. O ganho seria evitar um reprocessamento que a inbox já torna
inofensivo.

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

## 13. Observabilidade

Logs estruturados em JSON no stdout, carregando os identificadores disponíveis —
correlação, mensagem, transação, carteira, provedor. **Nunca** credencial, dado
sensível ou payload financeiro completo: log carrega identificador, não conteúdo.

Métricas expostas para coleta: resultados por status, duplicatas, retentativas,
DLQ, conflitos de concorrência, atraso da outbox, latência de processamento e
divergências de reconciliação.

**Em porta separada da API.** Métrica revela volume de operação, número de
carteiras e taxa de recusa — informação de negócio para quem souber ler. Na
porta da API, `/metrics` responde 401 sem token e 404 com token: a autenticação
roda antes do roteamento, então nem a existência da rota vaza.

**Nenhum rótulo carrega identificador.** Os rótulos são categorias fechadas —
tipo, status, código de falha, origem, worker, motivo. Um `walletId` em rótulo
criaria uma série por carteira e publicaria a lista delas para quem lesse
`/metrics`. A regra é conferida por teste, percorrendo a coleta e comparando
cada nome de rótulo contra uma lista permitida.

**O atraso da outbox é idade, não contagem.** Contar pendentes não distingue mil
eventos recém-gravados de um evento parado há uma hora, e é o segundo que indica
problema.

A camada de aplicação não conhece Prometheus: chama uma porta estreita, como faz
com persistência e transporte, e existe uma implementação nula para composição
sem observabilidade.

**Tracing OpenTelemetry, desligado por padrão.** Diferencial opcional do §12.
Sem configuração explícita o provedor é nulo: nenhum exportador, nenhuma
goroutine de envio, nenhum span alocado por requisição. O padrão é esse por
segurança antes de custo — um exportador ativo por engano manda dados de
operação para um endereço configurado em algum lugar.

O desligado ser um provedor nulo, e não um `if` em cada ponto instrumentado, é o
que mantém o custo em zero sem espalhar condicional pelo código.

Ligado, o trace cobre a requisição, as consultas do banco e as rodadas de
worker. O span é nomeado pelo **template** da rota, nunca pelo identificador —
mesma regra de cardinalidade fechada dos rótulos de métrica. Atributo de span
carrega identificador, nunca valor ou saldo: quem lê um trace não precisa saber
quanto foi apostado para diagnosticar latência.

**Log e trace se apontam.** Toda linha de log carrega `traceId` quando há span
válido no contexto, e o span de servidor carrega `correlation.id`. Sem esse par,
quem tem um não acha o outro.

**Propagação W3C, registrada mesmo desligada.** O propagador só lê e escreve
cabeçalho: um `traceparent` que atravessa este serviço continua válido para quem
está do outro lado. O `traceparent` recebido é dado externo, mas decide apenas o
identificador do trace — não autorização nem roteamento.

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
6. **Nem toda recusa vira registro.** O §7 exige `failureCode` estável em toda
   rejeição, e todas o têm. Mas há uma linha entre duas famílias:

   - **Recusa de negócio** — a operação endereça uma carteira válida e o negócio
     a nega: saldo insuficiente, reversão já aplicada. **É persistida** como
     `REJECTED`, com evento de rejeição. Sem isso o provedor reenviaria para
     sempre uma operação que já tem desfecho.
   - **Recusa de alvo** — a operação não endereça uma carteira válida: carteira
     inexistente, carteira de outro jogador, moeda divergente, valor incompatível
     com o tipo. **Não é persistida.** Uma linha que referencia uma carteira que
     a operação não pode tocar não é dado de auditoria, é entrada errada — e no
     caso da moeda o schema literalmente não a comporta, porque a chave
     estrangeira é composta por `(carteira, moeda)`.

   As duas devolvem 422 com o código de falha; a diferença está no que fica
   gravado. A recusa de alvo aparece no log, com o identificador de correlação.

7. **A referência do `WIN` é informativa: gravada, não resolvida.** O §7 permite
   que um ganho informe a aposta da mesma rodada como referência, mas só exige
   que ela seja *obrigatória e resolvida* em `REFUND` e `ROLLBACK`. São dois
   papéis sob o mesmo campo:

   - Em reversão a referência **aponta o que será desfeito**. É obrigatória, é
     resolvida para o identificador interno, precisa concordar em provedor,
     jogador, carteira, moeda e rodada, e a operação **espera por ela** se ainda
     não chegou.
   - Em `WIN` a referência **anota a que aposta o ganho pertence**. É opcional,
     fica gravada em `reference_external_transaction_id`, e
     `reference_transaction_id` permanece nulo.

   O ganho **não espera** pela aposta referenciada: credita na hora, mesmo que
   ela ainda não tenha chegado. Um ganho que esperasse deixaria de ser crédito
   imediato, e o §7 o classifica como crédito. O que garante isso não é uma
   condicional no caso de uso, e sim os guardas de `MarkPendingReference` e
   `ResolveReference`, ambos presos a reversão: aceitar o campo na construção não
   dá ao ganho nenhum caminho para virar pendência.

   Também não há validação da referência do ganho. Validar exigiria resolvê-la, e
   resolver traz de volta a espera que acabamos de descartar. `BET` e `LOSS`
   continuam sem poder carregar referência — recusados na fronteira, com 400
   apontando o campo, nas duas portas, porque a fila reusa a mesma decodificação.

   *Descartado — validar só quando a referência já existir:* pegaria erro de
   integração sem segurar crédito nenhum, mas faria a mesma requisição ser aceita
   ou recusada conforme a ordem de chegada das mensagens, que é justamente o que
   um sistema at-least-once não controla.

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
  handlers, mas a diferença existe: a instrumentação de tracing precisou de um
  portador de cabeçalhos próprio, porque o ecossistema OpenTelemetry pressupõe
  `http.Header`.
- **Reversão parcial não é suportada**, conforme o escopo definido.
- **A entrada por SQS não é autenticada por token.** O `providerId` vem do corpo,
  e a fronteira de confiança é a política de acesso da fila. Detalhado no §9;
  fica listado aqui porque é limitação do desenho, e não detalhe de
  implementação. A política é provisionada com as filas, mas o **LocalStack não
  a impõe**: localmente ela vale como declaração, e só um broker real a faz
  cumprir.
- **O dono do schema no ambiente local é superusuário**, porque é o usuário de
  bootstrap da imagem do PostgreSQL. Em produção o dono seria um papel comum, e
  o superusuário não seria usado por nenhum componente da aplicação. A
  separação que importa — a aplicação não ser dona — já vale nos dois casos.

## 16. Trabalho não concluído

Esta seção é mantida honesta ao longo do desenvolvimento. A coluna de estado do
`README.md` é a fonte precisa; aqui ficam as pendências que merecem comentário.

**Os dezesseis checkpoints do núcleo estão implementados e verificados.** O que
segue são diferenciais opcionais não feitos, e limitações declaradas.

- **Grafana, Loki e dashboards não foram feitos.** O §12 os trata como
  diferencial opcional. Ficam declarados como não feitos, e não meio feitos.
- **A contabilidade de partidas dobradas tem uma conta de casa por moeda, e mais
  nada.** Não há plano de contas, centro de custo nem contrapartida por
  provedor. É o suficiente para o balancete fechar, e é o que foi prometido.
- **O gatilho diferido pode abortar um commit financeiro.** É o comportamento
  correto — confirmar dinheiro cujos livros não fecham seria pior que falhar —,
  mas é uma forma nova de a transação falhar, introduzida por um diferencial
  opcional. Ela só dispara se alguém escrever partidas por fora do par.
- **O limite do `Money` vale por valor e não compõe para o agregado.** Cada
  saldo cabe em ±92.233.720.368.547.758,07, mas o balancete SOMA todas as
  carteiras de uma moeda, e duas perto do teto somam além dele. Quando isso
  acontece o relatório recusa por extenso, nomeando conta e moeda, em vez de
  devolver um total truncado — número errado num relatório de conferência é pior
  que relatório que se declara indisponível. A verificação de que os livros
  fecham não depende disso: ela é uma contagem, e continua respondendo. Achado
  numa passada de QA, junto com o estouro no crédito que virou
  `BALANCE_LIMIT_EXCEEDED`.
- **O trace não atravessa a outbox.** Uma operação HTTP e a publicação do evento
  que ela gerou são dois traces distintos, ligados apenas pelo `correlationId`
  que ambos carregam. Ligá-los exigiria persistir o `traceparent` na tabela da
  outbox — migration nova numa tabela do caminho financeiro — e o ganho não paga
  o risco num extra opcional. É decisão registrada, não esquecimento.
- O contador de conflito por versão de carteira é **inalcançável** no desenho
  atual: o lock da linha impede que a versão mude sob a transação. Ele existe
  como sintoma — se um dia subir, alguma escrita escapou do caminho travado.
- **A prosa do contrato de API pode envelhecer sem que nada quebre.** O gate e a
  validação cobrem rotas, códigos de falha e formas de resposta; descrições em
  texto não. É a mesma limitação do `README.md`, com o mesmo remédio.
- **O publicador da outbox faz uma chamada por evento.** Medido: ~202 eventos/s
  com lote de 500 a cada 500ms, contra ~761 eventos/s produzidos sob carga de
  130 req/s. O acúmulo é durável e não se perde, mas o atraso de integração
  cresce enquanto a carga durar. O próximo passo é `SendMessageBatch`, que
  agrupa até dez mensagens por chamada — não implementado para não mexer no
  caminho de publicação já verificado, e declarado aqui em vez de escondido.
- **O padrão de publicação é conservador**: lote de 50 a cada 2s, ou seja 25
  eventos/s. Serve a um ambiente pequeno e é gargalo em qualquer carga real. O
  compose eleva para 500 a cada 500ms; ambos são configuráveis.
- **As suítes com container são sensíveis a contenção.** Sob carga — outras
  coisas rodando na mesma máquina — a subida de um container pode estourar
  prazo, e um teste sem relação nenhuma falha por isso. Não há mitigação boa
  além de dar recurso à máquina.
- **A suíte de recuperação depende de `syscall.Kill` e grupos de processo**, o
  que a prende a sistemas do tipo Unix. Aceitável: o alvo de execução é Linux, e
  o compose já assume isso.
