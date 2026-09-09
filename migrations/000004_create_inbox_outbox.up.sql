-- Inbox e outbox.
--
-- A inbox garante que uma mensagem reentregue não seja reprocessada; a outbox
-- garante que nenhum evento seja publicado antes do commit que o originou.

CREATE TABLE inbox_messages (
    id            UUID        PRIMARY KEY,
    consumer_name TEXT        NOT NULL,
    -- Identidade durável da mensagem para este consumidor.
    message_id    TEXT        NOT NULL,
    payload_hash  BYTEA       NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Preenchido quando o tratamento é concluído de forma durável, no MESMO
    -- commit das alterações de domínio. É o que permite remover a mensagem da
    -- fila com segurança e reconhecer a reentrega sem reaplicar nada.
    completed_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX inbox_messages_consumer_message_uk
    ON inbox_messages (consumer_name, message_id);

CREATE TABLE outbox_events (
    -- Identidade estável do evento. Sobrevive a republicação: uma queda entre
    -- publicar e confirmar faz o evento sair de novo com o MESMO identificador,
    -- e é por ele que o consumidor deduplica.
    event_id       UUID        PRIMARY KEY,
    event_type     TEXT        NOT NULL,
    aggregate_type TEXT        NOT NULL,
    aggregate_id   UUID        NOT NULL,
    version        INT         NOT NULL,

    correlation_id TEXT,
    causation_id   TEXT,

    -- Instantâneo imutável no momento do commit. Um gatilho impede alteração:
    -- se o payload pudesse mudar depois, o evento publicado deixaria de
    -- descrever o que de fato aconteceu.
    payload        JSONB       NOT NULL,
    occurred_at    TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Publicação: tentativas, próximo envio e reivindicação por lease.
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    locked_by       TEXT,
    locked_until    TIMESTAMPTZ,
    last_error      TEXT,

    CONSTRAINT outbox_events_version_positive   CHECK (version >= 1),
    CONSTRAINT outbox_events_attempts_non_negative CHECK (attempts >= 0),
    -- Reivindicação é sempre um par: quem toma o registro se identifica e diz
    -- até quando. Sem isso não há como saber que um lease expirou.
    CONSTRAINT outbox_events_lease_pairing CHECK (
        (locked_by IS NULL) = (locked_until IS NULL)
    )
);

-- Fila do publicador. Só o que ainda não foi publicado interessa, então o
-- índice é parcial: ele não cresce com o histórico de eventos já enviados.
CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, event_id)
    WHERE published_at IS NULL;

CREATE INDEX outbox_events_aggregate_idx
    ON outbox_events (aggregate_type, aggregate_id, occurred_at);

-- O instantâneo é imutável; o controle de publicação não é.
CREATE OR REPLACE FUNCTION reject_outbox_payload_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.event_id     IS DISTINCT FROM OLD.event_id
    OR NEW.event_type   IS DISTINCT FROM OLD.event_type
    OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id
    OR NEW.version      IS DISTINCT FROM OLD.version
    OR NEW.payload      IS DISTINCT FROM OLD.payload
    OR NEW.occurred_at  IS DISTINCT FROM OLD.occurred_at THEN
        RAISE EXCEPTION
            'o instantaneo do evento e imutavel: altere apenas o controle de publicacao'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER outbox_events_snapshot_immutable
    BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION reject_outbox_payload_mutation();

COMMENT ON TABLE inbox_messages IS
    'Deduplicacao de mensagens recebidas. Conclusao compartilha o commit do dominio.';
COMMENT ON TABLE outbox_events IS
    'Eventos gravados no commit que os originou. Publicacao e trabalho de worker separado.';
