package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Lauiskk/munchkin/internal/app"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// walletRow é a linha da tabela.
//
// É uma struct SEPARADA do agregado, de propósito. O agregado tem campos não
// exportados, como o encapsulamento exige; o ORM precisa de campos exportados
// com tag. Os dois não cabem na mesma struct, e tentar acomodá-los levaria a
// abrir o agregado — que é como as invariantes deixam de ser invariantes.
//
// A tradução entre um e outro é explícita, por reidratação.
type walletRow struct {
	ID           uuid.UUID `gorm:"column:id;primaryKey"`
	PlayerID     uuid.UUID `gorm:"column:player_id"`
	Currency     string    `gorm:"column:currency"`
	BalanceMinor int64     `gorm:"column:balance_minor"`
	Version      int64     `gorm:"column:version"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

func (walletRow) TableName() string { return "wallets" }

// toDomain reidrata o agregado a partir da linha.
func (r walletRow) toDomain() (*wallet.Wallet, error) {
	currency, err := money.ParseCurrency(r.Currency)
	if err != nil {
		return nil, fmt.Errorf("carteira %s: %w", r.ID, err)
	}
	balance, err := money.New(r.BalanceMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("carteira %s: %w", r.ID, err)
	}
	return wallet.Rehydrate(
		wallet.ID(r.ID), wallet.PlayerID(r.PlayerID),
		balance, r.Version, r.CreatedAt, r.UpdatedAt,
	)
}

func rowFromWallet(w *wallet.Wallet) walletRow {
	return walletRow{
		ID:           uuid.UUID(w.ID()),
		PlayerID:     uuid.UUID(w.PlayerID()),
		Currency:     w.Currency().String(),
		BalanceMinor: w.Balance().Minor(),
		Version:      w.Version(),
		CreatedAt:    w.CreatedAt(),
		UpdatedAt:    w.UpdatedAt(),
	}
}

// WalletRepository persiste carteiras.
type WalletRepository struct{ db *Database }

// NewWalletRepository monta o repositório.
func NewWalletRepository(db *Database) *WalletRepository { return &WalletRepository{db: db} }

// Create insere uma carteira nova.
//
// Não consulta antes para saber se já existe: entre o SELECT e o INSERT outro
// processo insere, e a checagem prévia daria uma falsa sensação de segurança. O
// índice único decide, e o erro é traduzido para ErrConflict com o nome da
// constraint.
func (r *WalletRepository) Create(ctx context.Context, w *wallet.Wallet) error {
	row := rowFromWallet(w)
	if err := r.db.Session(ctx).Create(&row).Error; err != nil {
		classificado := classify(err)
		// A tradução acontece aqui, e não no caso de uso: é o repositório que
		// conhece os nomes das constraints da sua tabela. Deixá-la subir faria
		// a camada de aplicação importar o adaptador — que é o acoplamento que
		// esta arquitetura existe para evitar.
		if IsConstraint(classificado, ConstraintWalletPlayerCurrency) {
			return fmt.Errorf("%w: %s em %s",
				wallet.ErrAlreadyExists, w.PlayerID(), w.Currency())
		}
		return classificado
	}
	return nil
}

// FindByID busca uma carteira.
func (r *WalletRepository) FindByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error) {
	var row walletRow
	err := r.db.Session(ctx).
		Where("id = ?", uuid.UUID(id)).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: carteira %s", app.ErrNotFound, id)
	}
	if err != nil {
		return nil, classify(err)
	}
	return row.toDomain()
}

// LockByID lê a carteira travando a linha até o fim da transação.
//
// É o ponto de coordenação de todo o sistema: `FOR UPDATE` serializa os
// escritores DAQUELA carteira, e só dela — carteiras distintas seguem em
// paralelo. Exige transação aberta, porque um lock fora de transação é liberado
// imediatamente e não protege nada.
//
// O SQL é escrito à mão de propósito: quem revisa precisa ler o lock, não
// deduzi-lo de uma chamada de API fluente.
func (r *WalletRepository) LockByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error) {
	if !InTransaction(ctx) {
		return nil, fmt.Errorf("%w: LockByID", ErrNoTransaction)
	}

	var row walletRow
	err := r.db.Session(ctx).Raw(`
		SELECT id, player_id, currency, balance_minor, version, created_at, updated_at
		  FROM wallets
		 WHERE id = ?
		   FOR UPDATE`, uuid.UUID(id)).Scan(&row).Error
	if err != nil {
		return nil, classify(err)
	}
	if row.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: carteira %s", app.ErrNotFound, id)
	}
	return row.toDomain()
}

// UpdateBalance grava o saldo, exigindo que a versão esperada ainda valha.
//
// A cláusula `version = ?` é redundante com o lock — com ele em mãos ninguém
// mudou a linha — e existe como asserção: se ela falhar, o lock não foi tomado
// ou algo escreveu por fora, e queremos saber disso alto em vez de sobrescrever.
func (r *WalletRepository) UpdateBalance(ctx context.Context, w *wallet.Wallet, versaoAnterior int64) error {
	if !InTransaction(ctx) {
		return fmt.Errorf("%w: UpdateBalance", ErrNoTransaction)
	}

	res := r.db.Session(ctx).Exec(`
		UPDATE wallets
		   SET balance_minor = ?, version = ?, updated_at = ?
		 WHERE id = ? AND version = ?`,
		w.Balance().Minor(), w.Version(), w.UpdatedAt(),
		uuid.UUID(w.ID()), versaoAnterior)
	if res.Error != nil {
		return classify(res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("atualização de saldo afetou %d linhas para a carteira %s na versão %d",
			res.RowsAffected, w.ID(), versaoAnterior)
	}
	return nil
}

// Snapshot lê saldo, soma do ledger e contagem numa visão consistente.
//
// Uma instrução só. Sob READ COMMITTED cada instrução toma o seu instantâneo no
// início e a subconsulta enxerga o mesmo — então saldo e ledger são lidos do
// mesmo estado do banco. Em duas chamadas separadas, uma aposta entre elas
// compararia o saldo de um instante com o ledger de outro e acusaria
// divergência onde não há: um alarme que dispara sozinho é pior que alarme
// nenhum, porque ensina quem o atende a ignorá-lo.
//
// A soma é inteira, em unidades mínimas. Exata por construção, sem erro de
// arredondamento acumulado ao longo de um extrato inteiro.
func (r *WalletRepository) Snapshot(
	ctx context.Context, id wallet.ID,
) (appwallet.Snapshot, error) {
	var linha struct {
		StoredMinor     int64  `gorm:"column:stored_minor"`
		Currency        string `gorm:"column:currency"`
		CalculatedMinor int64  `gorm:"column:calculated_minor"`
		Entries         int64  `gorm:"column:entries"`
		Found           bool   `gorm:"column:found"`
	}

	err := r.db.Session(ctx).Raw(`
		SELECT w.balance_minor AS stored_minor,
		       w.currency      AS currency,
		       COALESCE(l.total, 0) AS calculated_minor,
		       COALESCE(l.n, 0)     AS entries,
		       true                 AS found
		  FROM wallets w
		  LEFT JOIN LATERAL (
		       SELECT SUM(CASE WHEN direction = 'CREDIT' THEN amount_minor
		                       ELSE -amount_minor END) AS total,
		              COUNT(*) AS n
		         FROM wallet_ledger_entries
		        WHERE wallet_id = w.id
		  ) l ON TRUE
		 WHERE w.id = ?`, uuid.UUID(id)).Scan(&linha).Error
	if err != nil {
		return appwallet.Snapshot{}, classify(err)
	}
	if !linha.Found {
		return appwallet.Snapshot{}, fmt.Errorf("%w: carteira %s", app.ErrNotFound, id)
	}

	moeda, err := money.ParseCurrency(linha.Currency)
	if err != nil {
		return appwallet.Snapshot{}, err
	}
	return appwallet.Snapshot{
		StoredMinor:     linha.StoredMinor,
		CalculatedMinor: linha.CalculatedMinor,
		Entries:         linha.Entries,
		Currency:        moeda,
	}, nil
}
