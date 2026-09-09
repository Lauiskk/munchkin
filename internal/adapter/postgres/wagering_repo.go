package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Lauiskk/munchkin/internal/app"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// transactionRow é a linha da tabela de transações.
//
// Os campos anuláveis usam os tipos de sql porque a distinção entre "vazio" e
// "ausente" é material aqui: provider_id vazio e provider_id nulo significam
// coisas diferentes, e a constraint de origem do schema depende disso.
type transactionRow struct {
	ID     uuid.UUID `gorm:"column:id;primaryKey"`
	Kind   string    `gorm:"column:kind"`
	Status string    `gorm:"column:status"`

	WalletID    uuid.UUID `gorm:"column:wallet_id"`
	PlayerID    uuid.UUID `gorm:"column:player_id"`
	AmountMinor int64     `gorm:"column:amount_minor"`
	Currency    string    `gorm:"column:currency"`

	ProviderID     sql.NullString `gorm:"column:provider_id"`
	ExternalID     sql.NullString `gorm:"column:external_transaction_id"`
	IdempotencyKey sql.NullString `gorm:"column:idempotency_key"`
	PayloadHash    []byte         `gorm:"column:payload_hash"`
	RoundID        sql.NullString `gorm:"column:round_id"`
	GameID         sql.NullString `gorm:"column:game_id"`

	ReferenceExternalID sql.NullString `gorm:"column:reference_external_transaction_id"`
	ReferenceID         *uuid.UUID     `gorm:"column:reference_transaction_id"`

	FailureCode        sql.NullString `gorm:"column:failure_code"`
	ResultBalanceMinor sql.NullInt64  `gorm:"column:result_balance_minor"`

	Attempts      int          `gorm:"column:attempts"`
	NextAttemptAt sql.NullTime `gorm:"column:next_attempt_at"`

	CreatedAt   time.Time    `gorm:"column:created_at"`
	UpdatedAt   time.Time    `gorm:"column:updated_at"`
	ProcessedAt sql.NullTime `gorm:"column:processed_at"`
}

func (transactionRow) TableName() string { return "wager_transactions" }

func rowFromTransaction(t *wagering.Transaction) transactionRow {
	row := transactionRow{
		ID:          uuid.UUID(t.ID()),
		Kind:        t.Kind().String(),
		Status:      t.Status().String(),
		WalletID:    uuid.UUID(t.WalletID()),
		PlayerID:    uuid.UUID(t.PlayerID()),
		AmountMinor: t.Amount().Minor(),
		Currency:    t.Amount().Currency().String(),
		Attempts:    t.Attempts(),
		CreatedAt:   t.CreatedAt(),
		UpdatedAt:   t.UpdatedAt(),
	}

	if o := t.Origin(); o != nil {
		row.ProviderID = texto(o.ProviderID.String())
		row.ExternalID = texto(o.ExternalID.String())
		row.IdempotencyKey = texto(o.IdempotencyKey)
		row.PayloadHash = o.PayloadHash
		row.RoundID = texto(o.RoundID.String())
		row.GameID = texto(o.GameID.String())
	}
	if ref := t.ReferenceExternalID(); ref != "" {
		row.ReferenceExternalID = texto(ref.String())
	}
	if id, ok := t.ReferenceID(); ok {
		u := uuid.UUID(id)
		row.ReferenceID = &u
	}
	if code := t.FailureCode(); code != "" {
		row.FailureCode = texto(code.String())
	}
	if saldo, ok := t.ResultBalance(); ok {
		row.ResultBalanceMinor = sql.NullInt64{Int64: saldo.Minor(), Valid: true}
	}
	if quando, ok := t.NextAttemptAt(); ok {
		row.NextAttemptAt = sql.NullTime{Time: quando, Valid: true}
	}
	if quando, ok := t.ProcessedAt(); ok {
		row.ProcessedAt = sql.NullTime{Time: quando, Valid: true}
	}
	return row
}

func (r transactionRow) toDomain() (*wagering.Transaction, error) {
	currency, err := money.ParseCurrency(r.Currency)
	if err != nil {
		return nil, fmt.Errorf("transação %s: %w", r.ID, err)
	}
	amount, err := money.New(r.AmountMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("transação %s: %w", r.ID, err)
	}

	estado := wagering.State{
		ID:        wagering.TransactionID(r.ID),
		Kind:      wagering.Kind(r.Kind),
		Status:    wagering.Status(r.Status),
		WalletID:  wallet.ID(r.WalletID),
		PlayerID:  wallet.PlayerID(r.PlayerID),
		Amount:    amount,
		Attempts:  r.Attempts,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}

	if r.ProviderID.Valid {
		estado.Origin = &wagering.ExternalOrigin{
			ProviderID:     wagering.ProviderID(r.ProviderID.String),
			ExternalID:     wagering.ExternalID(r.ExternalID.String),
			IdempotencyKey: r.IdempotencyKey.String,
			PayloadHash:    r.PayloadHash,
			RoundID:        wagering.RoundID(r.RoundID.String),
			GameID:         wagering.GameID(r.GameID.String),
		}
	}
	if r.ReferenceExternalID.Valid {
		estado.ReferenceExternalID = wagering.ExternalID(r.ReferenceExternalID.String)
	}
	if r.ReferenceID != nil {
		id := wagering.TransactionID(*r.ReferenceID)
		estado.ReferenceID = &id
	}
	if r.FailureCode.Valid {
		estado.FailureCode = wagering.FailureCode(r.FailureCode.String)
	}
	if r.ResultBalanceMinor.Valid {
		saldo, err := money.New(r.ResultBalanceMinor.Int64, currency)
		if err != nil {
			return nil, fmt.Errorf("transação %s: saldo resultante: %w", r.ID, err)
		}
		estado.ResultBalance = &saldo
	}
	if r.NextAttemptAt.Valid {
		estado.NextAttemptAt = &r.NextAttemptAt.Time
	}
	if r.ProcessedAt.Valid {
		estado.ProcessedAt = &r.ProcessedAt.Time
	}

	return wagering.Rehydrate(estado)
}

func texto(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// TransactionRepository persiste transações de aposta.
type TransactionRepository struct{ db *Database }

// NewTransactionRepository monta o repositório.
func NewTransactionRepository(db *Database) *TransactionRepository {
	return &TransactionRepository{db: db}
}

// Create insere a transação.
func (r *TransactionRepository) Create(ctx context.Context, t *wagering.Transaction) error {
	row := rowFromTransaction(t)
	if err := r.db.Session(ctx).Create(&row).Error; err != nil {
		return classify(err)
	}
	return nil
}

// FindByID busca uma transação pelo identificador interno.
func (r *TransactionRepository) FindByID(ctx context.Context, id wagering.TransactionID) (*wagering.Transaction, error) {
	var row transactionRow
	err := r.db.Session(ctx).Where("id = ?", uuid.UUID(id)).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: transação %s", app.ErrNotFound, id)
	}
	if err != nil {
		return nil, classify(err)
	}
	return row.toDomain()
}

// Claim tenta reivindicar a operação, inserindo-a.
//
// Este método É a verificação de idempotência. Não existe consulta prévia: o
// INSERT pergunta e responde numa ida só, e o índice único decide. Entre um
// SELECT e um INSERT outro processo insere — a consulta antes daria falsa
// segurança, e é justamente sob concorrência que ela falharia.
//
// Quando cinquenta requisições idênticas chegam juntas, quarenta e nove
// BLOQUEIAM no índice único até a primeira confirmar, e então recebem o conflito
// e leem o resultado dela. O PostgreSQL resolve a corrida; nenhum código nosso
// precisa coordenar nada.
func (r *TransactionRepository) Claim(ctx context.Context, t *wagering.Transaction) (appwagering.ClaimResult, error) {
	if !InTransaction(ctx) {
		return appwagering.ClaimResult{}, fmt.Errorf("%w: Claim", ErrNoTransaction)
	}

	row := rowFromTransaction(t)
	res := r.db.Session(ctx).Exec(`
		INSERT INTO wager_transactions
		  (id, kind, status, wallet_id, player_id, amount_minor, currency,
		   provider_id, external_transaction_id, idempotency_key, payload_hash,
		   round_id, game_id, reference_external_transaction_id,
		   attempts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		row.ID, row.Kind, row.Status, row.WalletID, row.PlayerID,
		row.AmountMinor, row.Currency,
		row.ProviderID, row.ExternalID, row.IdempotencyKey, row.PayloadHash,
		row.RoundID, row.GameID, row.ReferenceExternalID,
		row.Attempts, row.CreatedAt, row.UpdatedAt)

	if res.Error != nil {
		return appwagering.ClaimResult{}, classify(res.Error)
	}
	if res.RowsAffected == 1 {
		return appwagering.ClaimResult{Claimed: true}, nil
	}

	// Não inseriu: alguém já ocupa esta identidade. A linha está confirmada e
	// visível — o ON CONFLICT esperou o outro escritor terminar.
	origem := t.Origin()
	if origem == nil {
		return appwagering.ClaimResult{}, fmt.Errorf("conflito em operação sem origem externa: %s", t.ID())
	}

	existente, err := r.findConflicting(ctx, origem.ProviderID, origem.ExternalID, origem.IdempotencyKey)
	if err != nil {
		return appwagering.ClaimResult{}, err
	}
	return appwagering.ClaimResult{Claimed: false, Existing: existente}, nil
}

// findConflicting busca a transação que ocupa a identidade reivindicada.
//
// Procura primeiro pelo identificador externo, porque é ele que define a
// operação financeira — a chave identifica a tentativa. Se a operação já existe,
// é o registro dela que decide entre replay e conflito; só quando ela não existe
// é que a chave reutilizada para outra operação vira a explicação.
func (r *TransactionRepository) findConflicting(
	ctx context.Context, provider wagering.ProviderID, external wagering.ExternalID, key string,
) (*wagering.Transaction, error) {
	porOperacao, err := r.FindByProviderExternalID(ctx, provider, external)
	if err == nil {
		return porOperacao, nil
	}
	if !errors.Is(err, app.ErrNotFound) {
		return nil, err
	}

	var row transactionRow
	err = r.db.Session(ctx).Raw(`
		SELECT * FROM wager_transactions
		 WHERE provider_id = ? AND idempotency_key = ?`,
		string(provider), key).Scan(&row).Error
	if err != nil {
		return nil, classify(err)
	}
	if row.ID == uuid.Nil {
		// Conflito sem registro correspondente não deveria acontecer: o
		// ON CONFLICT espera o outro escritor confirmar antes de desistir.
		return nil, fmt.Errorf("conflito de idempotência sem registro correspondente para %s/%s",
			provider, external)
	}
	return row.toDomain()
}

// FindByProviderExternalID busca pela identidade da operação no provedor.
//
// O provedor faz parte da chave de busca, não é filtro aplicado depois: assim
// não existe caminho em que a consulta devolva uma linha de outro provedor e o
// filtro seja esquecido adiante.
func (r *TransactionRepository) FindByProviderExternalID(
	ctx context.Context, provider wagering.ProviderID, external wagering.ExternalID,
) (*wagering.Transaction, error) {
	var row transactionRow
	err := r.db.Session(ctx).Raw(`
		SELECT * FROM wager_transactions
		 WHERE provider_id = ? AND external_transaction_id = ?`,
		string(provider), string(external)).Scan(&row).Error
	if err != nil {
		return nil, classify(err)
	}
	if row.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: operação %s/%s", app.ErrNotFound, provider, external)
	}
	return row.toDomain()
}

// FindProcessedReversalOf busca a reversão concluída de uma referência.
//
// A cláusula reproduz exatamente o predicado de wager_transactions_single_reversal_uk
// (status PROCESSED e kind de reversão), então a consulta usa aquele índice e
// pergunta ao banco a mesma coisa que ele garante.
func (r *TransactionRepository) FindProcessedReversalOf(
	ctx context.Context, ref wagering.TransactionID,
) (*wagering.Transaction, error) {
	var row transactionRow
	err := r.db.Session(ctx).Raw(`
		SELECT * FROM wager_transactions
		 WHERE reference_transaction_id = ?
		   AND status = 'PROCESSED'
		   AND kind IN ('REFUND', 'ROLLBACK')`,
		uuid.UUID(ref)).Scan(&row).Error
	if err != nil {
		return nil, classify(err)
	}
	if row.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: reversão de %s", app.ErrNotFound, uuid.UUID(ref))
	}
	return row.toDomain()
}

// Settle grava o desfecho de uma transação.
//
// Só avança de PENDING ou PENDING_REFERENCE: a cláusula de estado na condição é
// a garantia de que um desfecho nunca sobrescreve outro já registrado. Se a
// atualização não afetar linha nenhuma, algo concluiu a transação por outro
// caminho, e isso precisa falhar alto em vez de sobrescrever em silêncio.
func (r *TransactionRepository) Settle(ctx context.Context, t *wagering.Transaction) error {
	if !InTransaction(ctx) {
		return fmt.Errorf("%w: Settle", ErrNoTransaction)
	}

	row := rowFromTransaction(t)
	res := r.db.Session(ctx).Exec(`
		UPDATE wager_transactions
		   SET status = ?, failure_code = ?, result_balance_minor = ?,
		       reference_transaction_id = ?, attempts = ?, next_attempt_at = ?,
		       processed_at = ?, updated_at = ?
		 WHERE id = ? AND status IN ('PENDING', 'PENDING_REFERENCE')`,
		row.Status, row.FailureCode, row.ResultBalanceMinor,
		row.ReferenceID, row.Attempts, row.NextAttemptAt,
		row.ProcessedAt, row.UpdatedAt, row.ID)

	if res.Error != nil {
		return classify(res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("desfecho da transação %s afetou %d linhas: o estado já era terminal",
			t.ID(), res.RowsAffected)
	}
	return nil
}
