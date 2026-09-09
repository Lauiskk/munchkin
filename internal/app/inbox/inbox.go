// Package inbox trata mensagens recebidas com garantia de efeito único.
//
// A garantia não vem do transporte. SQS entrega ao menos uma vez, e uma
// interrupção entre o commit e a remoção da mensagem produz reentrega — por
// desenho, não por defeito. O que torna a reentrega inofensiva é o registro da
// mensagem participar da MESMA transação das alterações de domínio: ou as duas
// coisas aconteceram, ou nenhuma aconteceu.
package inbox

import (
	"context"
	"fmt"
	"log/slog"

	"go.uber.org/fx"

	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
)

// ConsumerWagerTransactions é a identidade durável do consumidor de operações.
//
// Constante, e não configurável: mudá-la faria toda mensagem já tratada parecer
// nova, e o histórico da inbox deixaria de reconhecer a reentrega que existe
// para reconhecer.
const ConsumerWagerTransactions = "wager-transactions"

// TxManager delimita a transação.
type TxManager interface {
	Within(ctx context.Context, fn func(ctx context.Context) error) error
}

// ClaimResult descreve o que a inbox já sabia sobre a mensagem.
type ClaimResult struct {
	// Completed indica que um tratamento anterior chegou a commitar.
	Completed bool
	// SameHash indica que o conteúdo bate com o registrado da primeira vez.
	SameHash bool
}

// Repository é o que o tratamento precisa da persistência.
type Repository interface {
	// Claim registra a mensagem para este consumidor, ou devolve o que já
	// havia. Exige transação aberta: o registro e o efeito de domínio são
	// indivisíveis.
	Claim(ctx context.Context, consumidor, messageID string, hash []byte) (ClaimResult, error)
	// Complete marca o tratamento como durável.
	Complete(ctx context.Context, consumidor, messageID string) error
}

// Message é o que o transporte entrega, já validado.
type Message struct {
	ID    string
	Hash  []byte
	Input appwagering.Input
}

// Outcome diz ao transporte o que fazer com a mensagem.
type Outcome string

const (
	// Processed — tratada agora. Pode remover da fila.
	Processed Outcome = "PROCESSED"
	// Duplicate — já tinha sido tratada. Pode remover, sem reprocessar.
	Duplicate Outcome = "DUPLICATE"
	// HashMismatch — mesma identidade, conteúdo diferente. Mensagem inválida.
	HashMismatch Outcome = "HASH_MISMATCH"
)

// Handler trata uma mensagem de operação financeira.
type Handler struct {
	tx         TxManager
	repo       Repository
	processor  *appwagering.Processor
	log        *slog.Logger
	consumidor string
}

// NewHandler monta o tratamento.
//
// `consumidor` é a identidade durável do consumidor na inbox. Dois consumidores
// distintos podem tratar a mesma mensagem sem interferir um no outro, e é por
// isso que a unicidade é do par e não do messageId sozinho.
func NewHandler(
	tx TxManager, repo Repository, processor *appwagering.Processor,
	log *slog.Logger, consumidor string,
) *Handler {
	return &Handler{tx: tx, repo: repo, processor: processor, log: log, consumidor: consumidor}
}

// Handle trata a mensagem e devolve o que fazer com ela.
//
// O processamento financeiro é o MESMO caso de uso do HTTP. Não há um caminho
// de mensageria com regras próprias: o §10 exige que os dois compartilhem o
// caso de uso e as garantias, e um segundo caminho seria uma segunda chance de
// divergir.
func (h *Handler) Handle(ctx context.Context, m Message) (Outcome, appwagering.Output, error) {
	var desfecho Outcome
	var saida appwagering.Output

	err := h.tx.Within(ctx, func(ctx context.Context) error {
		reivindicada, err := h.repo.Claim(ctx, h.consumidor, m.ID, m.Hash)
		if err != nil {
			return fmt.Errorf("registro da mensagem %s: %w", m.ID, err)
		}

		if !reivindicada.SameHash {
			// Mesma identidade durável, conteúdo diferente. Tratar como
			// atualização seria aceitar que alguém reescreva o passado usando
			// um identificador já usado.
			desfecho = HashMismatch
			return nil
		}
		if reivindicada.Completed {
			// Reentrega do que já foi concluído. Não se reprocessa: o efeito
			// financeiro já está commitado, e repetir seria duplicar.
			desfecho = Duplicate
			return nil
		}

		// Chegar aqui com a mensagem já registrada e NÃO concluída significa
		// que um tratamento anterior morreu antes do commit. Reprocessar é
		// correto: nada daquele tratamento sobreviveu, e a idempotência
		// financeira segura o caso de ele ter commitado por outro caminho.
		saida, err = h.processor.Process(ctx, m.Input)
		if err != nil {
			return err
		}

		if err := h.repo.Complete(ctx, h.consumidor, m.ID); err != nil {
			return fmt.Errorf("conclusão da mensagem %s: %w", m.ID, err)
		}
		desfecho = Processed
		return nil
	})
	if err != nil {
		return "", appwagering.Output{}, err
	}
	return desfecho, saida, nil
}

// Module provê o tratamento de mensagens recebidas.
var Module = fx.Module("app.inbox",
	fx.Provide(newHandler),
)

func newHandler(
	tx TxManager, repo Repository, processor *appwagering.Processor, log *slog.Logger,
) *Handler {
	return NewHandler(tx, repo, processor, log, ConsumerWagerTransactions)
}
