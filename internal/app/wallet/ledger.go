package wallet

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	domain "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

const (
	// LedgerPageDefault é quantos lançamentos uma página traz sem pedido
	// explícito.
	LedgerPageDefault = 50
	// LedgerPageMax é o teto. Existe para que uma página não vire uma cópia da
	// tabela: sem teto, um `limit` grande transformaria a rota de extrato num
	// jeito barato de esgotar memória do processo.
	LedgerPageMax = 200
)

// Position marca onde uma página terminou.
//
// É o par ordenado da paginação por keyset. O `id` não é decoração: dois
// lançamentos podem cair no mesmo instante, e sem o desempate a página seguinte
// pularia um deles ou o repetiria.
type Position struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// LedgerPage é uma página do extrato.
type LedgerPage struct {
	Entries []ledger.Entry
	// Next é nil na última página. É assim que o chamador sabe parar, sem
	// precisar comparar tamanho de página com o limite pedido.
	Next *Position
}

// LedgerReader lê o extrato de uma carteira.
type LedgerReader interface {
	// ListByWallet devolve os lançamentos mais recentes primeiro, começando
	// depois de `after` quando ele existe.
	ListByWallet(ctx context.Context, id domain.ID, after *Position, limite int) ([]ledger.Entry, error)
}

// Statement devolve o extrato paginado de uma carteira.
type Statement struct {
	wallets Repository
	entries LedgerReader
}

// NewStatement monta a consulta de extrato.
func NewStatement(wallets Repository, entries LedgerReader) *Statement {
	return &Statement{wallets: wallets, entries: entries}
}

// List devolve uma página do extrato.
//
// A existência da carteira é conferida antes: sem isso, um identificador
// inexistente devolveria página vazia, e quem consulta não teria como
// distinguir "carteira sem movimento" de "carteira que não existe".
func (s *Statement) List(
	ctx context.Context, id domain.ID, after *Position, limite int,
) (LedgerPage, error) {
	if limite <= 0 {
		limite = LedgerPageDefault
	}
	if limite > LedgerPageMax {
		return LedgerPage{}, fmt.Errorf("limite acima do máximo de %d", LedgerPageMax)
	}

	if _, err := s.wallets.FindByID(ctx, id); err != nil {
		return LedgerPage{}, err
	}

	// Pede um a mais do que cabe na página. Se vier, existe página seguinte —
	// e isso é sabido sem uma segunda consulta de contagem, que sobre uma
	// tabela append-only seria cara e ficaria desatualizada no mesmo instante.
	linhas, err := s.entries.ListByWallet(ctx, id, after, limite+1)
	if err != nil {
		return LedgerPage{}, err
	}

	page := LedgerPage{Entries: linhas}
	if len(linhas) > limite {
		page.Entries = linhas[:limite]
		ultimo := page.Entries[limite-1]
		page.Next = &Position{CreatedAt: ultimo.CreatedAt(), ID: uuid.UUID(ultimo.ID())}
	}
	return page, nil
}
