-- Volta ao CHECK estrito, em que só reversão carrega referência.
--
-- O schema antigo não sabe representar um WIN com referência, então as linhas
-- que existirem precisam perder o campo antes — senão o ALTER falha e a
-- reversão fica impossível de aplicar.
--
-- Anular é aceitável aqui e não seria em outro campo: a referência do WIN é
-- METADADO INFORMATIVO. Ela liga o ganho à aposta da rodada e não participa de
-- nenhum cálculo — valor, saldo, lançamento e partidas ficam intactos. Nenhum
-- dinheiro se perde nesta reversão, e é por isso que ela pode acontecer.
--
-- O hash de idempotência daquelas operações foi calculado COM a referência, e
-- continua gravado em payload_hash. Um reenvio idêntico depois desta reversão
-- seria recusado pelo próprio CHECK antes de chegar à comparação de hash.
UPDATE wager_transactions
   SET reference_external_transaction_id = NULL
 WHERE kind = 'WIN'
   AND reference_external_transaction_id IS NOT NULL;

ALTER TABLE wager_transactions
    DROP CONSTRAINT wager_transactions_reference_by_kind;

ALTER TABLE wager_transactions
    ADD CONSTRAINT wager_transactions_reference_by_kind CHECK (
        (kind IN     ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL) OR
        (kind NOT IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NULL)
    );
