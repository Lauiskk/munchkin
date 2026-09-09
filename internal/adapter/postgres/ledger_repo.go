package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
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

// ledgerRow é a linha do extrato.
type ledgerRow struct {
	ID                 uuid.UUID `gorm:"column:id"`
	WalletID           uuid.UUID `gorm:"column:wallet_id"`
	TransactionID      uuid.UUID `gorm:"column:transaction_id"`
	Direction          string    `gorm:"column:direction"`
	AmountMinor        int64     `gorm:"column:amount_minor"`
	Currency           string    `gorm:"column:currency"`
	BalanceBeforeMinor int64     `gorm:"column:balance_before_minor"`
	BalanceAfterMinor  int64     `gorm:"column:balance_after_minor"`
	CreatedAt          time.Time `gorm:"column:created_at"`
}

// ListByWallet devolve uma página do extrato, do mais recente para o mais
// antigo.
//
// A paginação é por keyset — `(created_at, id) < (?, ?)` — e não por OFFSET.
// OFFSET tem dois defeitos aqui, e o segundo é o grave: o banco varre e
// descarta tudo que já passou, então a página mil custa mil páginas; e, se um
// lançamento entrar entre duas requisições, as páginas seguintes deslocam, e o
// cliente pula ou repete linhas sem perceber. Num extrato financeiro, pular uma
// linha é pior que ser lento.
//
// A ordem é a mesma do índice wallet_ledger_entries_wallet_created_idx, criado
// na etapa 04 exatamente para esta consulta.
func (r *LedgerRepository) ListByWallet(
	ctx context.Context, id wallet.ID, after *appwallet.Position, limite int,
) ([]ledger.Entry, error) {
	var linhas []ledgerRow
	var err error

	const selecao = `
		SELECT * FROM wallet_ledger_entries
		 WHERE wallet_id = ?`
	const ordem = `
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`

	if after == nil {
		err = r.db.Session(ctx).Raw(selecao+ordem, uuid.UUID(id), limite).Scan(&linhas).Error
	} else {
		// A comparação é do PAR, não de cada coluna em separado: comparar só
		// created_at perderia o desempate quando dois lançamentos caem no mesmo
		// instante, e a página seguinte repetiria ou pularia um deles.
		err = r.db.Session(ctx).Raw(
			selecao+`
		   AND (created_at, id) < (?, ?)`+ordem,
			uuid.UUID(id), after.CreatedAt, after.ID, limite).Scan(&linhas).Error
	}
	if err != nil {
		return nil, classify(err)
	}

	entradas := make([]ledger.Entry, 0, len(linhas))
	for _, l := range linhas {
		e, err := l.toDomain()
		if err != nil {
			return nil, err
		}
		entradas = append(entradas, e)
	}
	return entradas, nil
}

// toDomain reconstrói o lançamento passando pelo construtor do domínio.
//
// Poderia montar a struct direto e seria mais rápido. Passar pelo construtor
// revalida a aritmética na leitura — o banco já a impõe por CHECK, então isto é
// a quarta camada, e a única que pegaria uma linha escrita por fora do sistema.
func (l ledgerRow) toDomain() (ledger.Entry, error) {
	moeda, err := money.ParseCurrency(l.Currency)
	if err != nil {
		return ledger.Entry{}, err
	}
	valor, err := money.New(l.AmountMinor, moeda)
	if err != nil {
		return ledger.Entry{}, err
	}
	antes, err := money.New(l.BalanceBeforeMinor, moeda)
	if err != nil {
		return ledger.Entry{}, err
	}
	depois, err := money.New(l.BalanceAfterMinor, moeda)
	if err != nil {
		return ledger.Entry{}, err
	}

	return ledger.New(
		ledger.ID(l.ID), wallet.ID(l.WalletID), wagering.TransactionID(l.TransactionID),
		wallet.Direction(l.Direction), valor, antes, depois, l.CreatedAt)
}
