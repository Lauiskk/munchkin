package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/Lauiskk/munchkin/internal/domain/ledger"
)

// LedgerRepository grava lançamentos.
//
// Só existe Append. Não há Update nem Delete, e a ausência é deliberada: o
// ledger é append-only, e um método que não existe é mais difícil de chamar por
// engano do que um método que existe e falha. O banco recusaria de qualquer
// forma, por gatilho e por privilégio revogado — esta é a terceira camada, a
// que impede o código de sequer tentar.
type LedgerRepository struct{ db *Database }

// NewLedgerRepository monta o repositório.
func NewLedgerRepository(db *Database) *LedgerRepository { return &LedgerRepository{db: db} }

// Append grava um lançamento.
//
// O SQL é escrito à mão: este é o registro que a auditoria financeira lê, e quem
// revisa precisa ver exatamente quais colunas são gravadas.
func (r *LedgerRepository) Append(ctx context.Context, e ledger.Entry) error {
	if !InTransaction(ctx) {
		// Um lançamento fora da transação que moveu o saldo seria confirmado
		// sozinho, e o ledger passaria a discordar da carteira.
		return fmt.Errorf("%w: Append de lançamento", ErrNoTransaction)
	}

	err := r.db.Session(ctx).Exec(`
		INSERT INTO wallet_ledger_entries
		  (id, wallet_id, transaction_id, direction, amount_minor, currency,
		   balance_before_minor, balance_after_minor, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.UUID(e.ID()), uuid.UUID(e.WalletID()), uuid.UUID(e.TransactionID()),
		string(e.Direction()), e.Amount().Minor(), e.Amount().Currency().String(),
		e.BalanceBefore().Minor(), e.BalanceAfter().Minor(), e.CreatedAt(),
	).Error

	return classify(err)
}

// SumSigned devolve a soma dos lançamentos da carteira, com sinal.
//
// É a base da reconciliação: créditos menos débitos tem de dar o saldo
// armazenado. A soma é inteira, em unidades mínimas — exata por construção, sem
// erro de arredondamento acumulado.
func (r *LedgerRepository) SumSigned(ctx context.Context, walletID uuid.UUID) (int64, int64, error) {
	var resultado struct {
		Total   int64
		Entries int64
	}
	err := r.db.Session(ctx).Raw(`
		SELECT
		  COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount_minor
		                    ELSE -amount_minor END), 0) AS total,
		  COUNT(*)                                       AS entries
		  FROM wallet_ledger_entries
		 WHERE wallet_id = ?`, walletID).Scan(&resultado).Error
	if err != nil {
		return 0, 0, classify(err)
	}
	return resultado.Total, resultado.Entries, nil
}
