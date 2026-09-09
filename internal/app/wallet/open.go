package wallet

import (
	"context"
	"fmt"
	"time"

	"github.com/Lauiskk/munchkin/internal/app"
	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	domain "github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/correlation"
)

// OpenInput é a entrada do caso de uso.
type OpenInput struct {
	PlayerID       domain.PlayerID
	InitialBalance money.Money
}

// OpenOutput é o resultado.
type OpenOutput struct {
	Wallet *domain.Wallet
}

// Opener abre carteiras.
type Opener struct {
	tx           TxManager
	wallets      Repository
	transactions TransactionRepository
	entries      LedgerRepository
	outbox       OutboxRepository
	clock        app.Clock
}

// NewOpener monta o caso de uso.
func NewOpener(
	tx TxManager, wallets Repository, transactions TransactionRepository,
	entries LedgerRepository, outbox OutboxRepository, clock app.Clock,
) *Opener {
	return &Opener{
		tx: tx, wallets: wallets, transactions: transactions,
		entries: entries, outbox: outbox, clock: clock,
	}
}

// Open abre uma carteira.
//
// Tudo num commit só: carteira, transação de abertura, lançamento de crédito e
// os dois eventos. Ou a abertura inteira valeu, ou nenhuma parte dela valeu —
// não pode restar carteira sem a transação que a creditou, nem evento
// anunciando um crédito que não aconteceu.
//
// Saldo inicial zero grava apenas a carteira: sem movimentação não há lançamento
// nem evento financeiro, conforme o contrato.
func (o *Opener) Open(ctx context.Context, in OpenInput) (OpenOutput, error) {
	if in.PlayerID.IsZero() {
		return OpenOutput{}, fmt.Errorf("%w: jogador ausente", domain.ErrInvalidID)
	}
	if !in.InitialBalance.IsValid() {
		return OpenOutput{}, fmt.Errorf("%w: saldo inicial ausente", money.ErrUninitialized)
	}

	// O identificador é gerado pelo servidor, nunca aceito do chamador: aceitá-lo
	// permitiria colisão deliberada e sondagem de identificadores existentes.
	walletID, err := domain.NewID()
	if err != nil {
		return OpenOutput{}, err
	}

	agora := o.clock.Now()
	w, movimento, err := domain.Open(walletID, in.PlayerID, in.InitialBalance, agora)
	if err != nil {
		return OpenOutput{}, err
	}

	err = o.tx.Within(ctx, func(ctx context.Context) error {
		// A unicidade é decidida pelo índice único, não por uma consulta
		// prévia: entre o SELECT e o INSERT outro processo insere, e a checagem
		// antes daria falsa segurança. O repositório traduz a violação em
		// wallet.ErrAlreadyExists.
		if err := o.wallets.Create(ctx, w); err != nil {
			return err
		}

		if movimento == nil {
			return nil
		}
		return o.registrarAbertura(ctx, w, *movimento, agora)
	})
	if err != nil {
		return OpenOutput{}, err
	}

	return OpenOutput{Wallet: w}, nil
}

// registrarAbertura grava a transação OPENING, seu lançamento e os eventos.
func (o *Opener) registrarAbertura(
	ctx context.Context, w *domain.Wallet, movimento domain.Movement, agora time.Time,
) error {
	txID, err := wagering.NewTransactionID()
	if err != nil {
		return err
	}

	abertura, err := wagering.NewOpening(txID, w.ID(), w.PlayerID(), movimento.Amount, agora)
	if err != nil {
		return err
	}
	if err := abertura.MarkProcessed(w.Balance(), agora); err != nil {
		return err
	}
	if err := o.transactions.Create(ctx, abertura); err != nil {
		return err
	}

	entryID, err := ledger.NewID()
	if err != nil {
		return err
	}
	entrada, err := ledger.FromMovement(entryID, w.ID(), txID, movimento, agora)
	if err != nil {
		return err
	}
	if err := o.entries.Append(ctx, entrada); err != nil {
		return err
	}

	corr := correlation.From(ctx)

	if err := o.enfileirar(ctx, event.WagerTransactionProcessed{
		TransactionID: txID,
		WalletID:      w.ID(),
		PlayerID:      w.PlayerID(),
		Kind:          wagering.Opening,
		Money:         movimento.Amount,
		Balance:       w.Balance(),
		ProcessedAt:   event.Timestamp(agora),
	}, corr); err != nil {
		return err
	}

	return o.enfileirar(ctx, event.WalletBalanceChanged{
		WalletID:      w.ID(),
		TransactionID: txID,
		Direction:     movimento.Direction,
		Money:         movimento.Amount,
		BalanceBefore: movimento.BalanceBefore,
		BalanceAfter:  movimento.BalanceAfter,
		WalletVersion: w.Version(),
		ChangedAt:     event.Timestamp(agora),
	}, corr)
}

func (o *Opener) enfileirar(ctx context.Context, payload event.Payload, correlationID string) error {
	id, err := event.NewID()
	if err != nil {
		return err
	}
	env, err := event.New(id, payload, correlationID, "", o.clock.Now())
	if err != nil {
		return err
	}
	return o.outbox.Enqueue(ctx, env)
}
