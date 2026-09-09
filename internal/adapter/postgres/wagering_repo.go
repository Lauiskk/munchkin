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
