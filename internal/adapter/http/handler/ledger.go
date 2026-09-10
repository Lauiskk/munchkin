package handler

import (
	"net/http"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/adapter/http/dto"
	appledger "github.com/Lauiskk/munchkin/internal/app/ledger"
)

// Ledger responde pela contabilidade de partidas dobradas.
//
// Separado do handler de carteira porque a visão é GLOBAL: o balancete não
// pertence a nenhuma carteira, e pendurá-lo em `/wallets/:id` sugeriria que
// pertence.
type Ledger struct{ balancer *appledger.TrialBalancer }

// NewLedger monta o handler.
func NewLedger(balancer *appledger.TrialBalancer) *Ledger {
	return &Ledger{balancer: balancer}
}

// TrialBalance devolve o balancete das partidas dobradas.
//
//	GET /ledger/trial-balance   (escopo wallets:admin)
//
// Escopo de administração, e não de leitura de provedor: o balancete soma a
// plataforma inteira. Um integrador com `wagering:read` não pode ver o total
// movimentado — mesmo que a resposta não nomeie carteira nenhuma.
func (h *Ledger) TrialBalance(c *fiber.Ctx) error {
	resultado, err := h.balancer.Run(c.UserContext())
	if err != nil {
		return err
	}
	return c.Status(http.StatusOK).JSON(dto.NewTrialBalanceResponse(resultado))
}
