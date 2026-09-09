-- Transações de aposta.
--
-- Registra tanto a abertura interna de carteira (OPENING) quanto as operações
-- externas dos provedores. As restrições abaixo são o que impede que uma
-- origem se disfarce da outra.

CREATE TABLE wager_transactions (
    id           UUID    PRIMARY KEY,
    kind         TEXT    NOT NULL,
    status       TEXT    NOT NULL,

    wallet_id    UUID    NOT NULL,
    player_id    UUID    NOT NULL,

    amount_minor BIGINT  NOT NULL,
    currency     CHAR(3) NOT NULL,

    -- Metadados de origem externa. Nulos para OPENING, obrigatórios para o
    -- restante — a restrição wager_transactions_origin, abaixo, impõe isso.
    provider_id             TEXT,
    external_transaction_id TEXT,
    idempotency_key         TEXT,
    -- SHA-256 do JSON canônico dos campos de negócio.
    payload_hash            BYTEA,
    round_id                TEXT,
    game_id                 TEXT,

    -- Reversões. O identificador externo é o que o provedor envia; o interno é
    -- preenchido quando a referência é resolvida.
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID REFERENCES wager_transactions (id),

    -- Resultado.
    failure_code         TEXT,
    result_balance_minor BIGINT,

    -- Controle da espera por referência ainda indisponível.
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,

    -- A moeda da movimentação tem de ser a da carteira. Chave estrangeira
    -- composta em vez de validação no código: assim a divergência não é algo
    -- que alguém precise lembrar de conferir, é algo que o banco recusa.
    CONSTRAINT wager_transactions_wallet_currency_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),

    CONSTRAINT wager_transactions_kind CHECK (
        kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')
    ),
    CONSTRAINT wager_transactions_status CHECK (
        status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')
    ),
    CONSTRAINT wager_transactions_currency_iso4217 CHECK (currency ~ '^[A-Z]{3}$'),

    -- LOSS é a única operação de valor zero; as demais exigem valor positivo.
    CONSTRAINT wager_transactions_amount_by_kind CHECK (
        (kind =  'LOSS' AND amount_minor =  0) OR
        (kind <> 'LOSS' AND amount_minor >  0)
    ),

    -- Separa origem interna de externa. Sem isto, um provedor poderia enviar
    -- OPENING por HTTP e creditar a própria carteira.
    CONSTRAINT wager_transactions_origin CHECK (
        (kind =  'OPENING'
            AND provider_id             IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key         IS NULL
            AND payload_hash            IS NULL
            AND round_id                IS NULL
            AND game_id                 IS NULL)
        OR
        (kind <> 'OPENING'
            AND provider_id             IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key         IS NOT NULL
            AND payload_hash            IS NOT NULL
            AND round_id                IS NOT NULL
            AND game_id                 IS NOT NULL)
    ),

    -- Reversão exige referência; o que não é reversão não pode ter.
    CONSTRAINT wager_transactions_reference_by_kind CHECK (
        (kind IN     ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL) OR
        (kind NOT IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NULL)
    ),

    -- Estado terminal de recusa exige código de falha, e só ele pode tê-lo.
    -- Toda rejeição precisa ser auditável por um código estável.
    CONSTRAINT wager_transactions_failure_code CHECK (
        (status IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)
    ),

    -- Conclusão com sucesso exige o saldo observado no processamento, que é o
    -- que o replay devolve mesmo depois de a carteira ter se movimentado.
    CONSTRAINT wager_transactions_processed_result CHECK (
        (status = 'PROCESSED') = (result_balance_minor IS NOT NULL AND processed_at IS NOT NULL)
    ),

    CONSTRAINT wager_transactions_attempts_non_negative CHECK (attempts >= 0)
);

-- A operação financeira é única por provedor. Este índice é a própria
-- verificação de idempotência: o INSERT com ON CONFLICT pergunta e responde
-- numa ida só, e é ele que serializa cinquenta envios simultâneos da mesma
-- aposta até o primeiro confirmar.
CREATE UNIQUE INDEX wager_transactions_provider_external_uk
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE provider_id IS NOT NULL;

-- A chave de idempotência também é única por provedor. Ter os dois índices
-- permite distinguir "mesma chave, conteúdo diferente" de "mesma operação com
-- outra chave" — situações que o contrato precisa diferenciar.
CREATE UNIQUE INDEX wager_transactions_provider_key_uk
    ON wager_transactions (provider_id, idempotency_key)
    WHERE provider_id IS NOT NULL;

-- Uma única abertura por carteira: impede crédito inicial duplicado.
CREATE UNIQUE INDEX wager_transactions_opening_uk
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

-- No máximo UMA reversão bem-sucedida por referência.
--
-- Interpretação adotada: o enunciado pede que uma referência não receba duas
-- reversões do mesmo tipo, e pede também que REFUND e ROLLBACK sobre a mesma
-- aposta não devolvam o mesmo débito duas vezes. A leitura literal da primeira
-- frase permitiria um de cada, o que contraria a segunda. Vale a regra forte.
CREATE UNIQUE INDEX wager_transactions_single_reversal_uk
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Fila do worker que retenta resolver referências pendentes.
CREATE INDEX wager_transactions_pending_reference_idx
    ON wager_transactions (next_attempt_at)
    WHERE status = 'PENDING_REFERENCE';

-- Consulta paginada por provedor, na ordem estável usada pelo cursor.
CREATE INDEX wager_transactions_provider_created_idx
    ON wager_transactions (provider_id, created_at DESC, id DESC)
    WHERE provider_id IS NOT NULL;

COMMENT ON TABLE wager_transactions IS
    'Operações financeiras. OPENING é interna; as demais vêm de provedores.';
COMMENT ON COLUMN wager_transactions.payload_hash IS
    'SHA-256 do JSON canônico dos campos de negócio, sem a chave de idempotência.';
COMMENT ON COLUMN wager_transactions.result_balance_minor IS
    'Saldo observado no processamento original; é o que o replay devolve.';
