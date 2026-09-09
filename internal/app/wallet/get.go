package wallet

import (
	"context"

	domain "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// Getter consulta carteiras.
type Getter struct{ wallets Repository }

// NewGetter monta o caso de uso.
func NewGetter(wallets Repository) *Getter { return &Getter{wallets: wallets} }

// Get devolve a carteira, ou app.ErrNotFound.
//
// Não abre transação: é leitura de uma linha só, e envolvê-la numa transação
// não acrescentaria garantia nenhuma — só seguraria uma conexão a mais.
func (g *Getter) Get(ctx context.Context, id domain.ID) (*domain.Wallet, error) {
	return g.wallets.FindByID(ctx, id)
}
