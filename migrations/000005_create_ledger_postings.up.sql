-- Partidas dobradas — diferencial opcional do §6.4.
--
-- Tabela PARALELA: `wallet_ledger_entries` não é tocada. A tabela que o §6.4
-- especifica já está verificada, e um diferencial opcional não é motivo para
-- mexer nela.
--
-- Cada lançamento vira um par de partidas que se anulam: a carteira de um lado,
-- a casa do outro. É daí que sai o balancete — e é por isso que a soma de tudo,
-- por moeda, tem de dar exatamente zero.

CREATE TABLE ledger_postings (
    id             UUID    PRIMARY KEY,
    transaction_id UUID    NOT NULL REFERENCES wager_transactions (id),

    account_kind   TEXT    NOT NULL,
    -- Preenchido só na conta da carteira. A conta da casa é identificada pela
    -- moeda: uma casa por moeda, porque moedas diferentes não se somam.
    wallet_id      UUID    NULL,
    currency       CHAR(3) NOT NULL,

    -- COM SINAL. A invariante desta tabela é "a soma dá zero", e uma soma sobre
    -- coluna com sinal é a expressão direta dessa frase. Com direção mais valor
    -- positivo, a mesma checagem viraria um CASE — correto, e menos legível
    -- justamente onde a legibilidade importa.
    amount_minor   BIGINT  NOT NULL,

    created_at     TIMESTAMPTZ NOT NULL,

    CONSTRAINT ledger_postings_account_kind
        CHECK (account_kind IN ('WALLET', 'HOUSE')),

    -- A carteira tem identidade; a casa não. Amarrar as duas coisas impede a
    -- linha meio preenchida: conta de carteira sem carteira, ou conta da casa
    -- pendurada numa.
    CONSTRAINT ledger_postings_account_shape CHECK (
        (account_kind = 'WALLET' AND wallet_id IS NOT NULL) OR
        (account_kind = 'HOUSE'  AND wallet_id IS NULL)
    ),

    -- Partida de valor zero não é lançamento, é ruído.
    CONSTRAINT ledger_postings_amount_not_zero CHECK (amount_minor <> 0),

    CONSTRAINT ledger_postings_wallet_currency_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency)
);

-- Um par por transação, e não mais. Índices PARCIAIS porque `wallet_id` é nulo
-- na conta da casa, e NULL não colide com NULL num índice único comum — o que
-- deixaria a casa sem proteção nenhuma contra duplicata.
CREATE UNIQUE INDEX ledger_postings_wallet_side_uk
    ON ledger_postings (transaction_id, wallet_id)
 WHERE account_kind = 'WALLET';

CREATE UNIQUE INDEX ledger_postings_house_side_uk
    ON ledger_postings (transaction_id, currency)
 WHERE account_kind = 'HOUSE';

-- O balancete agrupa por moeda e tipo de conta. É o único índice de leitura
-- daqui: não há extrato de partidas por carteira, e índice que nenhuma consulta
-- usa é custo de escrita no caminho do dinheiro em troca de nada.
CREATE INDEX ledger_postings_currency_kind_idx
    ON ledger_postings (currency, account_kind);

-- ---------------------------------------------------------------------------
-- O histórico entra junto
-- ---------------------------------------------------------------------------
--
-- Um invariante que só vale do deploy em diante deixa metade dos dados fora
-- dele, e o balancete de uma base já em uso nasceria errado. As partidas são
-- uma projeção determinística do lançamento — dá para reconstruir a história
-- inteira sem adivinhar nada.

INSERT INTO ledger_postings
      (id, transaction_id, account_kind, wallet_id, currency, amount_minor, created_at)
SELECT gen_random_uuid(), e.transaction_id, 'WALLET', e.wallet_id, e.currency,
       CASE WHEN e.direction = 'CREDIT' THEN e.amount_minor ELSE -e.amount_minor END,
       e.created_at
  FROM wallet_ledger_entries e;

INSERT INTO ledger_postings
      (id, transaction_id, account_kind, wallet_id, currency, amount_minor, created_at)
SELECT gen_random_uuid(), e.transaction_id, 'HOUSE', NULL, e.currency,
       CASE WHEN e.direction = 'CREDIT' THEN -e.amount_minor ELSE e.amount_minor END,
       e.created_at
  FROM wallet_ledger_entries e;

-- O preenchimento é conferido pela MESMA regra que vai valer para a aplicação,
-- só que numa passada de agregado em vez de linha a linha. Não é atalho: é a
-- ferramenta certa para o tamanho do trabalho. Com o gatilho de linha ligado
-- durante o preenchimento, esta migration levou 5m38s numa base de 34.678
-- lançamentos — 69 mil somas indexadas, uma por linha inserida.
DO $$
DECLARE quebradas BIGINT;
BEGIN
    SELECT COUNT(*) INTO quebradas FROM (
        SELECT transaction_id
          FROM ledger_postings
         GROUP BY transaction_id
        HAVING SUM(amount_minor) <> 0
            OR COUNT(*) < 2
            OR COUNT(DISTINCT currency) <> 1
    ) q;

    IF quebradas > 0 THEN
        RAISE EXCEPTION
            'preenchimento deixou % transacao(oes) sem par que fecha', quebradas
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Os livros fecham — conferido no COMMIT, não no meio da transação
-- ---------------------------------------------------------------------------
--
-- DIFERIDO porque as duas metades podem chegar em INSTRUÇÕES diferentes da
-- mesma transação. Medido no PostgreSQL 18: um gatilho AFTER ROW imediato ainda
-- enxerga as duas linhas quando elas vêm numa instrução só — ele dispara no fim
-- da instrução, não no fim da linha — mas recusa a metade que chega sozinha,
-- mesmo que a outra venha logo depois e a transação feche.
--
-- Como a aplicação escreve o par numa instrução só, ela não precisaria do
-- diferimento. Quem precisa é o caminho que alguém escrever amanhã: o momento
-- certo de perguntar "os livros fecham?" é o fim da TRANSAÇÃO.
--
-- Criado DEPOIS do preenchimento, e é o que o mantém barato: o gatilho é a
-- ferramenta certa para uma transação que escreve duas linhas, e a errada para
-- uma que escreve sessenta e nove mil.
--
-- Isto não existe para o código de hoje, que só escreve pelo par. Existe para o
-- caminho que alguém escrever amanhã.
CREATE OR REPLACE FUNCTION assert_postings_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    sobra   BIGINT;
    moedas  BIGINT;
    linhas  BIGINT;
BEGIN
    SELECT COALESCE(SUM(amount_minor), 0), COUNT(DISTINCT currency), COUNT(*)
      INTO sobra, moedas, linhas
      FROM ledger_postings
     WHERE transaction_id = NEW.transaction_id;

    IF linhas < 2 THEN
        RAISE EXCEPTION
            'partida solitaria na transacao %: partida dobrada exige as duas metades',
            NEW.transaction_id
            USING ERRCODE = 'check_violation';
    END IF;

    IF moedas <> 1 THEN
        RAISE EXCEPTION
            'partidas em % moedas na transacao %: dinheiro de moedas diferentes nao se soma',
            moedas, NEW.transaction_id
            USING ERRCODE = 'check_violation';
    END IF;

    IF sobra <> 0 THEN
        RAISE EXCEPTION
            'os livros nao fecham na transacao %: sobra de % em unidades minimas',
            NEW.transaction_id, sobra
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_postings_balanced
    AFTER INSERT ON ledger_postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_postings_balanced();

-- ---------------------------------------------------------------------------
-- Append-only — as MESMAS duas camadas do ledger
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION reject_posting_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION
        'ledger_postings e append-only: % nao e permitido. Correcao contabil exige partidas novas.',
        TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER ledger_postings_no_update_delete
    BEFORE UPDATE OR DELETE ON ledger_postings
    FOR EACH ROW EXECUTE FUNCTION reject_posting_mutation();

CREATE TRIGGER ledger_postings_no_truncate
    BEFORE TRUNCATE ON ledger_postings
    FOR EACH STATEMENT EXECUTE FUNCTION reject_posting_mutation();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'munchkin_app') THEN
        REVOKE UPDATE, DELETE, TRUNCATE ON ledger_postings FROM munchkin_app;
    END IF;
END;
$$;

COMMENT ON TABLE ledger_postings IS
    'Partidas dobradas. Projecao do ledger: cada lancamento vira um par que soma zero.';
