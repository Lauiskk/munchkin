-- O §7 do enunciado permite que um WIN informe a aposta da mesma rodada como
-- referência. O CHECK original só admitia referência em REFUND e ROLLBACK, e
-- por isso um WIN legítimo era recusado pelo banco.
--
-- A regra passa a ter três braços em vez de dois:
--
--   REFUND, ROLLBACK  → referência OBRIGATÓRIA (é o que será desfeito)
--   WIN               → referência OPCIONAL    (informativa, liga o ganho à rodada)
--   BET, LOSS, OPENING→ referência PROIBIDA    (não há o que referenciar)
--
-- O terceiro braço é o que continua protegendo o resolvedor de pendências: sem
-- ele, uma aposta com referência entraria e alguém tentaria resolvê-la.
--
-- reference_transaction_id (a referência interna já resolvida) segue como está e
-- permanece NULL em WIN: ele grava a referência, não a resolve. O índice único
-- de reversão bem-sucedida também não muda — ele já é parcial em
-- kind IN ('REFUND','ROLLBACK'), então um WIN com referência não disputa com
-- reversão nenhuma.

ALTER TABLE wager_transactions
    DROP CONSTRAINT wager_transactions_reference_by_kind;

ALTER TABLE wager_transactions
    ADD CONSTRAINT wager_transactions_reference_by_kind CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR
        (kind = 'WIN')
        OR
        (kind NOT IN ('REFUND', 'ROLLBACK', 'WIN')
            AND reference_external_transaction_id IS NULL)
    );
