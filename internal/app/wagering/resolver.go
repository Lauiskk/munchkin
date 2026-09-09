package wagering

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Lauiskk/munchkin/internal/app"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

// Política de retomada.
//
// O backoff cresce exponencialmente e tem teto: sem teto, uma pendência de
// longa duração acabaria com intervalos de horas e a resolução chegaria muito
// depois de a referência ter aparecido. O limite de tentativas é o TTL efetivo —
// esgotado, a operação termina como recusada, em vez de ficar pendente para
// sempre ocupando a fila.
const (
	MaxResolveAttempts = 8
	backoffBase        = 5 * time.Second
	backoffMax         = 5 * time.Minute
)

// PendingReference identifica uma pendência a resolver.
type PendingReference struct {
	TransactionID uuid.UUID
	WalletID      uuid.UUID
}

// PendingRepository é o que o resolvedor precisa da persistência.
type PendingRepository interface {
	// FindDuePendingReferences lista candidatas SEM travar.
	FindDuePendingReferences(ctx context.Context, agora time.Time, limite int) ([]PendingReference, error)
	// LockPendingReference trava a pendência, ou devolve não encontrado quando
	// outra instância já a tomou.
	LockPendingReference(ctx context.Context, id uuid.UUID, agora time.Time) (*domain.Transaction, error)
}

// Resolver retoma reversões que aguardam sua referência.
//
// Roda em TODAS as instâncias, sem eleição de líder. A coordenação é
// SKIP LOCKED: quem chegar primeiro numa pendência a resolve, e os demais
// seguem para a próxima em vez de esperar.
type Resolver struct {
	tx        TxManager
	pending   PendingRepository
	wallets   WalletRepository
	processor *Processor
	log       *slog.Logger
	clock     app.Clock
	lote      int
}

// NewResolver monta o resolvedor.
func NewResolver(
	tx TxManager, pending PendingRepository, wallets WalletRepository,
	processor *Processor, log *slog.Logger, clock app.Clock,
) *Resolver {
	return &Resolver{
		tx: tx, pending: pending, wallets: wallets,
		processor: processor, log: log, clock: clock, lote: 50,
	}
}

// RunOnce processa uma rodada e devolve quantas pendências foram tratadas.
func (r *Resolver) RunOnce(ctx context.Context) (int, error) {
	agora := r.clock.Now()

	candidatas, err := r.pending.FindDuePendingReferences(ctx, agora, r.lote)
	if err != nil {
		return 0, err
	}

	tratadas := 0
	for _, c := range candidatas {
		if ctx.Err() != nil {
			return tratadas, ctx.Err()
		}
		feito, err := r.resolverUma(ctx, c, agora)
		if err != nil {
			// Uma pendência que falha não pode parar a fila: a próxima pode
			// estar perfeitamente resolvível, e parar aqui faria uma linha
			// problemática travar todas as outras.
			r.log.LogAttrs(ctx, slog.LevelError, "resolver.failed",
				slog.String(logs.KeyTransactionID, c.TransactionID.String()),
				slog.String(logs.KeyError, err.Error()))
			continue
		}
		if feito {
			tratadas++
		}
	}
	return tratadas, nil
}

// resolverUma trata uma pendência dentro da sua própria transação.
func (r *Resolver) resolverUma(ctx context.Context, c PendingReference, agora time.Time) (bool, error) {
	feito := false

	err := r.tx.Within(ctx, func(ctx context.Context) error {
		// A ordem dos locks é a MESMA do fluxo principal: carteira primeiro,
		// transação depois. Invertê-la abriria deadlock com um provedor que
		// reenviasse a mesma reversão por HTTP enquanto o worker a resolve.
		w, err := r.wallets.LockByID(ctx, wallet.ID(c.WalletID))
		if err != nil {
			return err
		}

		operacao, err := r.pending.LockPendingReference(ctx, c.TransactionID, agora)
		if errors.Is(err, app.ErrNotFound) {
			// Outra instância já tomou esta pendência, ou ela deixou de estar
			// vencida. Não é erro: é a coordenação funcionando.
			return nil
		}
		if err != nil {
			return err
		}

		feito = true
		return r.aplicarRetomada(ctx, w, operacao, agora)
	})

	return feito, err
}

// aplicarRetomada tenta concluir a reversão, ou reagenda, ou desiste.
func (r *Resolver) aplicarRetomada(
	ctx context.Context, w *wallet.Wallet, operacao *domain.Transaction, agora time.Time,
) error {
	// Mesma razão do fluxo síncrono: decidir muta a carteira, então a versão
	// anterior é capturada antes.
	versaoAnterior := w.Version()

	d, err := r.processor.decidir(ctx, w, operacao, agora)
	if err != nil {
		return err
	}

	if d.aguardar {
		tentativas := operacao.Attempts() + 1
		if tentativas >= MaxResolveAttempts {
			// TTL esgotado. Terminar como recusada é melhor que ficar pendente
			// para sempre: o provedor recebe um desfecho e a fila não cresce
			// indefinidamente.
			r.log.LogAttrs(ctx, slog.LevelWarn, "resolver.gave_up",
				slog.String(logs.KeyTransactionID, operacao.ID().String()),
				slog.Int("attempts", tentativas))

			if err := operacao.MarkRejected(domain.FailureReferenceNotFound, agora); err != nil {
				return err
			}
			if err := r.processor.transactions.Settle(ctx, operacao); err != nil {
				return err
			}
			return r.processor.publicarRecusa(ctx, operacao, agora)
		}

		proxima := agora.Add(Backoff(tentativas))
		if err := operacao.ScheduleRetry(proxima, agora); err != nil {
			return err
		}
		return r.processor.transactions.Settle(ctx, operacao)
	}

	// A referência apareceu: o desfecho é o mesmo que teria sido no fluxo
	// síncrono. Reaproveitar o caminho evita que a retomada e a entrada direta
	// divirjam — que é como um sistema passa a dar respostas diferentes para a
	// mesma operação conforme o caminho por onde ela entrou.
	_, err = r.processor.aplicarDecisao(ctx, w, operacao, d, versaoAnterior, agora)
	return err
}

// Backoff devolve a espera até a tentativa indicada, com teto.
//
// O crescimento é por deslocamento de bits, não por exponenciação em ponto
// flutuante. A primeira versão usava math.Pow e o gate de float a recusou — com
// razão: este pacote está no caminho do dinheiro, e a regra não abre exceção
// para "mas aqui não é valor monetário". A versão inteira, além de passar, é
// exata e não tem o que arredondar.
func Backoff(tentativa int) time.Duration {
	if tentativa < 1 {
		tentativa = 1
	}

	// Limita o deslocamento antes de aplicá-lo: deslocar um int64 além de 62
	// posições é comportamento indefinido em termos de resultado útil, e
	// chegaríamos ao teto muito antes disso de qualquer forma.
	const maxDeslocamento = 20
	deslocamento := tentativa - 1
	if deslocamento > maxDeslocamento {
		return backoffMax
	}

	espera := backoffBase << deslocamento
	if espera > backoffMax || espera <= 0 {
		return backoffMax
	}
	return espera
}
