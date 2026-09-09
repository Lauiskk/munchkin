-- Lançamentos do ledger.
--
-- Append-only imposto pelo banco em duas camadas independentes: privilégio
-- revogado do papel da aplicação, e gatilho que recusa a operação mesmo que o
-- privilégio seja reconcedido por engano. Uma camada só seria uma camada a
-- menos do que o necessário para o critério de auditabilidade.

CREATE TABLE wallet_ledger_entries (
    id             UUID    PRIMARY KEY,
    wallet_id      UUID    NOT NULL,
    transaction_id UUID    NOT NULL REFERENCES wager_transactions (id),

    direction      TEXT    NOT NULL,
    amount_minor   BIGINT  NOT NULL,
    currency       CHAR(3) NOT NULL,

    balance_before_minor BIGINT NOT NULL,
    balance_after_minor  BIGINT NOT NULL,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT wallet_ledger_entries_wallet_currency_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),

    CONSTRAINT wallet_ledger_entries_direction CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT wallet_ledger_entries_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT wallet_ledger_entries_balances_non_negative CHECK (
        balance_before_minor >= 0 AND balance_after_minor >= 0
    ),

    -- A aritmética do lançamento é conferida pelo banco. É a invariante central
    -- do ledger: um lançamento que não fecha a conta não entra, e por isso a
    -- reconciliação pode confiar na soma dos lançamentos.
    CONSTRAINT wallet_ledger_entries_arithmetic CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor) OR
        (direction = 'DEBIT'  AND balance_after_minor = balance_before_minor - amount_minor)
    )
);

-- Um lançamento por transação e carteira: é o que impede movimentação
-- duplicada mesmo que a mesma transação seja aplicada duas vezes.
CREATE UNIQUE INDEX wallet_ledger_entries_wallet_transaction_uk
    ON wallet_ledger_entries (wallet_id, transaction_id);

-- Paginação por cursor com ordenação estável.
CREATE INDEX wallet_ledger_entries_wallet_created_idx
    ON wallet_ledger_entries (wallet_id, created_at DESC, id DESC);

-- Camada 1: o gatilho. Vale para qualquer papel, inclusive o dono.
CREATE OR REPLACE FUNCTION reject_ledger_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION
        'wallet_ledger_entries e append-only: % nao e permitido. Correcao financeira exige lancamento novo.',
        TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER wallet_ledger_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_mutation();

CREATE TRIGGER wallet_ledger_entries_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION reject_ledger_mutation();

-- Camada 2: o privilégio. O papel da aplicação recebe SELECT e INSERT e perde
-- o resto. Funciona porque a aplicação NÃO é dona do schema — para o dono, o
-- privilégio vem de ownership e revogar não teria efeito.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'munchkin_app') THEN
        REVOKE UPDATE, DELETE, TRUNCATE ON wallet_ledger_entries FROM munchkin_app;
    END IF;
END;
$$;

COMMENT ON TABLE wallet_ledger_entries IS
    'Ledger append-only. Correcao financeira exige lancamento novo, nunca edicao.';
