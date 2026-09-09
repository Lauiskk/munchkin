-- Carteiras.
--
-- As invariantes ficam no schema, não no código. Um caminho de aplicação com
-- defeito é recusado pelo banco; a garantia não depende de a aplicação estar
-- correta.

CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL,

    -- Dinheiro em unidades mínimas. BIGINT, nunca NUMERIC e nunca REAL:
    -- soma e comparação inteiras são exatas, e é sobre esta coluna que o
    -- UPDATE condicionado do caminho quente opera.
    balance_minor BIGINT      NOT NULL,

    -- Versão do agregado. Começa em 1 e só avança quando o saldo muda.
    version       BIGINT      NOT NULL,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A última linha de defesa contra saldo negativo. Mesmo que um caminho
    -- futuro esqueça o lock, o banco recusa.
    CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    CONSTRAINT wallets_version_positive     CHECK (version >= 1),
    CONSTRAINT wallets_currency_iso4217     CHECK (currency ~ '^[A-Z]{3}$'),

    -- Redundante como chave, proposital como alvo: permite que transações e
    -- lançamentos referenciem (wallet_id, currency) por chave estrangeira
    -- composta. Com isso, movimentação em moeda diferente da carteira deixa de
    -- ser algo que o domínio precisa lembrar de validar e passa a ser algo que
    -- o banco não aceita.
    CONSTRAINT wallets_id_currency_uk UNIQUE (id, currency)
);

-- Uma carteira por par (jogador, moeda).
CREATE UNIQUE INDEX wallets_player_currency_uk ON wallets (player_id, currency);

COMMENT ON TABLE  wallets IS 'Raiz do agregado financeiro. Saldo em unidades mínimas.';
COMMENT ON COLUMN wallets.balance_minor IS 'Saldo em unidades mínimas da moeda (centavos para BRL).';
COMMENT ON COLUMN wallets.version IS 'Versão do agregado; avança apenas quando o saldo muda.';
