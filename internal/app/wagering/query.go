package wagering

import (
	"context"
	"fmt"

	"github.com/Lauiskk/munchkin/internal/app"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
)

// Querier consulta operações.
type Querier struct{ transactions TransactionRepository }

// NewQuerier monta o caso de uso.
func NewQuerier(transactions TransactionRepository) *Querier {
	return &Querier{transactions: transactions}
}

// ByID busca pela identidade interna, restrita ao provedor do chamador.
//
// O provedor é conferido DEPOIS da busca, e a divergência devolve "não
// encontrado" em vez de "proibido": um 403 confirmaria que a transação existe,
// e a existência já é informação. É o mesmo motivo de o enunciado exigir
// isolamento inclusive em replay.
func (q *Querier) ByID(
	ctx context.Context, id domain.TransactionID, chamador domain.ProviderID,
) (*domain.Transaction, error) {
	t, err := q.transactions.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !pertenceAo(t, chamador) {
		return nil, fmt.Errorf("%w: transação %s", app.ErrNotFound, id)
	}
	return t, nil
}

// ByExternalID busca pela identidade da operação no provedor.
//
// O provedor da busca é sempre o do chamador, nunca o do caminho da URL: aceitar
// o da URL permitiria a qualquer provedor consultar transações de outro apenas
// trocando um segmento do endereço.
func (q *Querier) ByExternalID(
	ctx context.Context, chamador domain.ProviderID, external domain.ExternalID,
) (*domain.Transaction, error) {
	return q.transactions.FindByProviderExternalID(ctx, chamador, external)
}

func pertenceAo(t *domain.Transaction, chamador domain.ProviderID) bool {
	origem := t.Origin()
	return origem != nil && origem.ProviderID == chamador
}
