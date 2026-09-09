package wagering

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Lauiskk/munchkin/internal/app"
	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/correlation"
)

var (
	// ErrIdempotencyConflict indica reuso de chave com conteúdo diferente, ou
	// a mesma operação enviada com outra chave. Nada é gravado.
	ErrIdempotencyConflict = errors.New("conflito de idempotência")
	// ErrProviderMismatch indica corpo divergindo do provedor do token.
	ErrProviderMismatch = errors.New("o provedor do corpo difere do token")
)

// Input é a operação recebida, já validada na fronteira.
type Input struct {
	// ProviderID vem do TOKEN, nunca do corpo. É o que sustenta o isolamento
	// entre provedores.
	ProviderID     domain.ProviderID
	ExternalID     domain.ExternalID
	IdempotencyKey string

	PlayerID wallet.PlayerID
	WalletID wallet.ID
	RoundID  domain.RoundID
	GameID   domain.GameID
	Kind     domain.Kind
	Money    money.Money

	ReferenceExternalID domain.ExternalID
}

// Output é o desfecho, do ponto de vista do provedor.
//
// Recusa de negócio NÃO é erro aqui: ela é um desfecho legítimo, com registro
// persistido e evento emitido. Erro fica reservado para conflito de idempotência
// e falha de infraestrutura.
type Output struct {
	TransactionID    domain.TransactionID
	Status           domain.Status
	Balance          money.Money
	FailureCode      domain.FailureCode
	IdempotentReplay bool
}

// Processor executa operações financeiras.
type Processor struct {
	tx           TxManager
	wallets      WalletRepository
	transactions TransactionRepository
	entries      LedgerRepository
	outbox       OutboxRepository
	clock        app.Clock
}

// NewProcessor monta o caso de uso.
func NewProcessor(
	tx TxManager, wallets WalletRepository, transactions TransactionRepository,
	entries LedgerRepository, outbox OutboxRepository, clock app.Clock,
) *Processor {
	return &Processor{
		tx: tx, wallets: wallets, transactions: transactions,
		entries: entries, outbox: outbox, clock: clock,
	}
}

// Process aplica a operação.
//
// Todo o trabalho acontece numa transação SQL, nesta ordem, que é fixa e
// importa:
//
//  1. trava a linha da carteira — serializa os escritores DAQUELA carteira;
//  2. valida o alvo (existe, moeda, jogador);
//  3. reivindica a idempotência inserindo a operação;
//  4. o domínio decide;
//  5. grava saldo, lançamento, desfecho e eventos;
//  6. commita — e só aqui os eventos passam a existir.
//
// A carteira é travada ANTES da reivindicação porque a operação referencia a
// carteira por chave estrangeira: uma operação para carteira inexistente não
// pode ser inserida, então precisa ser recusada antes da tentativa. O custo é
// que um replay também toma o lock; a alternativa seria uma consulta extra no
// caminho de toda operação nova, que é o caminho quente.
func (p *Processor) Process(ctx context.Context, in Input) (Output, error) {
	fingerprint := domain.Fingerprint{
		ProviderID:          in.ProviderID,
		ExternalID:          in.ExternalID,
		PlayerID:            in.PlayerID,
		WalletID:            in.WalletID,
		RoundID:             in.RoundID,
		GameID:              in.GameID,
		Kind:                in.Kind,
		Amount:              in.Money,
		ReferenceExternalID: in.ReferenceExternalID,
	}
	hash, err := fingerprint.Hash()
	if err != nil {
		return Output{}, err
	}

	var out Output
	err = p.tx.Within(ctx, func(ctx context.Context) error {
		var erroInterno error
		out, erroInterno = p.executar(ctx, in, hash)
		return erroInterno
	})
	return out, err
}

func (p *Processor) executar(ctx context.Context, in Input, hash []byte) (Output, error) {
	agora := p.clock.Now()

	// 1. Trava a carteira. Daqui até o commit, nenhuma outra transação mexe
	//    nesta carteira — e carteiras distintas seguem em paralelo.
	w, err := p.wallets.LockByID(ctx, in.WalletID)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return recusaSemRegistro(domain.FailureWalletNotFound), nil
		}
		return Output{}, err
	}

	// 2. Valida o alvo. Estas recusas não são gravadas: uma operação que não
	//    endereça uma carteira válida não é dado de auditoria, é entrada
	//    errada — e, no caso da moeda, o schema literalmente não a comporta,
	//    porque a chave estrangeira é composta por (carteira, moeda).
	if !w.BelongsTo(in.PlayerID) {
		return recusaSemRegistro(domain.FailureWalletPlayerMismatch), nil
	}
	if in.Money.Currency() != w.Currency() {
		return recusaSemRegistro(domain.FailureCurrencyMismatch), nil
	}

	// 3. Reivindica a idempotência. O INSERT é a verificação.
	txID, err := domain.NewTransactionID()
	if err != nil {
		return Output{}, err
	}
	operacao, err := domain.NewExternal(txID, in.Kind, w.ID(), w.PlayerID(), in.Money,
		domain.ExternalOrigin{
			ProviderID:     in.ProviderID,
			ExternalID:     in.ExternalID,
			IdempotencyKey: in.IdempotencyKey,
			PayloadHash:    hash,
			RoundID:        in.RoundID,
			GameID:         in.GameID,
		}, in.ReferenceExternalID, agora)
	if err != nil {
		// Valor incompatível com o tipo é recusa de negócio, não erro.
		if errors.Is(err, domain.ErrInvalidAmountForKind) {
			return recusaSemRegistro(domain.FailureInvalidAmountForKind), nil
		}
		return Output{}, err
	}

	claim, err := p.transactions.Claim(ctx, operacao)
	if err != nil {
		return Output{}, err
	}
	if !claim.Claimed {
		return p.decidirReplay(claim.Existing, in, hash)
	}

	// 4 e 5. O domínio decide e o resultado é gravado.
	return p.aplicar(ctx, w, operacao, agora)
}

// aplicar executa a movimentação e grava tudo.
func (p *Processor) aplicar(
	ctx context.Context, w *wallet.Wallet, operacao *domain.Transaction, agora time.Time,
) (Output, error) {
	versaoAnterior := w.Version()

	movimento, recusa, err := p.movimentar(w, operacao, agora)
	if err != nil {
		return Output{}, err
	}

	if recusa != "" {
		// Recusa de negócio COMMITA. O registro terminal e o evento precisam
		// sobreviver, senão o provedor reenviaria para sempre uma operação que
		// já tem desfecho. Como nada foi escrito no saldo, basta gravar o
		// desfecho — não é preciso savepoint.
		if err := operacao.MarkRejected(recusa, agora); err != nil {
			return Output{}, err
		}
		if err := p.transactions.Settle(ctx, operacao); err != nil {
			return Output{}, err
		}
		if err := p.publicarRecusa(ctx, operacao, agora); err != nil {
			return Output{}, err
		}
		return Output{
			TransactionID: operacao.ID(),
			Status:        domain.Rejected,
			Balance:       w.Balance(),
			FailureCode:   recusa,
		}, nil
	}

	if movimento != nil {
		if err := p.wallets.UpdateBalance(ctx, w, versaoAnterior); err != nil {
			return Output{}, err
		}
		entryID, err := ledger.NewID()
		if err != nil {
			return Output{}, err
		}
		entrada, err := ledger.FromMovement(entryID, w.ID(), operacao.ID(), *movimento, agora)
		if err != nil {
			return Output{}, err
		}
		if err := p.entries.Append(ctx, entrada); err != nil {
			return Output{}, err
		}
	}

	if err := operacao.MarkProcessed(w.Balance(), agora); err != nil {
		return Output{}, err
	}
	if err := p.transactions.Settle(ctx, operacao); err != nil {
		return Output{}, err
	}
	if err := p.publicarConclusao(ctx, w, operacao, movimento, agora); err != nil {
		return Output{}, err
	}

	return Output{
		TransactionID: operacao.ID(),
		Status:        domain.Processed,
		Balance:       w.Balance(),
	}, nil
}

// movimentar pede ao domínio a movimentação correspondente ao tipo.
//
// Devolve (movimento, "", nil) quando aplicou, (nil, código, nil) quando o
// domínio recusou, e (nil, "", erro) quando algo inesperado aconteceu.
func (p *Processor) movimentar(
	w *wallet.Wallet, operacao *domain.Transaction, agora time.Time,
) (*wallet.Movement, domain.FailureCode, error) {
	switch operacao.Kind() {
	case domain.Bet:
		mv, err := w.Debit(operacao.Amount(), agora)
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return nil, domain.FailureInsufficientFunds, nil
		}
		if err != nil {
			return nil, "", err
		}
		return &mv, "", nil

	case domain.Win:
		mv, err := w.Credit(operacao.Amount(), agora)
		if err != nil {
			return nil, "", err
		}
		return &mv, "", nil

	case domain.Loss:
		// LOSS conclui sem movimentar: o dinheiro já saiu na aposta. Sem
		// movimentação não há lançamento, e a versão da carteira não avança.
		return nil, "", nil

	default:
		return nil, "", fmt.Errorf("tipo %s não é tratado nesta etapa", operacao.Kind())
	}
}

// decidirReplay resolve o que fazer quando a identidade já estava ocupada.
func (p *Processor) decidirReplay(existente *domain.Transaction, in Input, hash []byte) (Output, error) {
	origem := existente.Origin()
	if origem == nil {
		return Output{}, fmt.Errorf("conflito com operação interna %s", existente.ID())
	}

	// A operação já foi aplicada com OUTRA chave. Aceitar seria permitir que a
	// mesma movimentação financeira fosse reaplicada trocando a chave.
	if origem.IdempotencyKey != in.IdempotencyKey {
		return Output{}, fmt.Errorf(
			"%w: a operação %s já foi registrada com outra chave de idempotência",
			ErrIdempotencyConflict, in.ExternalID)
	}

	// Mesma chave, conteúdo diferente.
	if !bytes.Equal(origem.PayloadHash, hash) {
		return Output{}, fmt.Errorf(
			"%w: a chave %s já foi usada com conteúdo diferente",
			ErrIdempotencyConflict, in.IdempotencyKey)
	}

	// Replay legítimo: devolve o resultado persistido, sem reaplicar nada. O
	// saldo é o OBSERVADO NO PROCESSAMENTO ORIGINAL, mesmo que a carteira já
	// tenha se movimentado depois — é o que o contrato promete.
	saldo, temSaldo := existente.ResultBalance()
	if !temSaldo {
		saldo = in.Money.ZeroOfSame()
	}
	return Output{
		TransactionID:    existente.ID(),
		Status:           existente.Status(),
		Balance:          saldo,
		FailureCode:      existente.FailureCode(),
		IdempotentReplay: true,
	}, nil
}

// recusaSemRegistro monta o desfecho de uma recusa que não é persistida.
func recusaSemRegistro(code domain.FailureCode) Output {
	return Output{Status: domain.Rejected, FailureCode: code}
}

func (p *Processor) publicarConclusao(
	ctx context.Context, w *wallet.Wallet, operacao *domain.Transaction,
	movimento *wallet.Movement, agora time.Time,
) error {
	origem := operacao.Origin()
	corr := correlation.From(ctx)

	if err := p.enfileirar(ctx, event.WagerTransactionProcessed{
		TransactionID: operacao.ID(),
		WalletID:      w.ID(),
		PlayerID:      w.PlayerID(),
		Kind:          operacao.Kind(),
		Money:         operacao.Amount(),
		Balance:       w.Balance(),
		ProviderID:    origem.ProviderID,
		ExternalID:    origem.ExternalID,
		RoundID:       origem.RoundID,
		GameID:        origem.GameID,
		ProcessedAt:   agora,
	}, corr); err != nil {
		return err
	}

	// LOSS conclui sem alterar o saldo, então não produz este evento. Emiti-lo
	// mesmo assim faria um consumidor registrar uma mudança que não houve.
	if movimento == nil {
		return nil
	}

	return p.enfileirar(ctx, event.WalletBalanceChanged{
		WalletID:      w.ID(),
		TransactionID: operacao.ID(),
		Direction:     movimento.Direction,
		Money:         movimento.Amount,
		BalanceBefore: movimento.BalanceBefore,
		BalanceAfter:  movimento.BalanceAfter,
		WalletVersion: w.Version(),
		ChangedAt:     agora,
	}, corr)
}

func (p *Processor) publicarRecusa(ctx context.Context, operacao *domain.Transaction, agora time.Time) error {
	origem := operacao.Origin()
	return p.enfileirar(ctx, event.WagerTransactionRejected{
		TransactionID: operacao.ID(),
		WalletID:      operacao.WalletID(),
		PlayerID:      operacao.PlayerID(),
		Kind:          operacao.Kind(),
		Money:         operacao.Amount(),
		FailureCode:   operacao.FailureCode(),
		ProviderID:    origem.ProviderID,
		ExternalID:    origem.ExternalID,
		RejectedAt:    agora,
	}, correlation.From(ctx))
}

func (p *Processor) enfileirar(ctx context.Context, payload event.Payload, correlationID string) error {
	id, err := event.NewID()
	if err != nil {
		return err
	}
	env, err := event.New(id, payload, correlationID, "", p.clock.Now())
	if err != nil {
		return err
	}
	return p.outbox.Enqueue(ctx, env)
}
