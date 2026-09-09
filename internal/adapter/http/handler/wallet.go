package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/adapter/contract"
	"github.com/Lauiskk/munchkin/internal/adapter/http/dto"
	"github.com/Lauiskk/munchkin/internal/app"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

// Wallet responde pelas rotas de carteira.
type Wallet struct {
	opener     *appwallet.Opener
	getter     *appwallet.Getter
	statement  *appwallet.Statement
	reconciler *appwallet.Reconciler
}

// NewWallet monta o handler.
func NewWallet(
	opener *appwallet.Opener, getter *appwallet.Getter,
	statement *appwallet.Statement, reconciler *appwallet.Reconciler,
) *Wallet {
	return &Wallet{opener: opener, getter: getter, statement: statement, reconciler: reconciler}
}

// Open abre uma carteira.
//
//	POST /wallets   (escopo wallets:admin)
func (h *Wallet) Open(c *fiber.Ctx) error {
	var req dto.OpenWalletRequest
	if err := dto.Bind(c, &req); err != nil {
		return err
	}

	playerID, saldoInicial, err := req.Decode()
	if err != nil {
		return err
	}

	out, err := h.opener.Open(c.UserContext(), appwallet.OpenInput{
		PlayerID:       playerID,
		InitialBalance: saldoInicial,
	})
	if err != nil {
		return traduzirErroDeCarteira(err)
	}

	return c.Status(http.StatusCreated).JSON(dto.NewWalletResponse(out.Wallet))
}

// Get devolve uma carteira.
//
//	GET /wallets/:walletId   (escopo wallets:admin)
func (h *Wallet) Get(c *fiber.Ctx) error {
	id, err := identificadorDeCarteira(c)
	if err != nil {
		return err
	}

	w, err := h.getter.Get(c.UserContext(), id)
	if err != nil {
		return traduzirErroDeCarteira(err)
	}

	return c.Status(http.StatusOK).JSON(dto.NewWalletResponse(w))
}

// traduzirErroDeCarteira converte erro de domínio ou de aplicação em erro HTTP.
//
// A tradução vive no adaptador, e não no caso de uso: o caso de uso não deve
// saber que existe HTTP. Quando a mesma operação for exposta por outra porta —
// a fila, na etapa 11 —, ela reaproveita o caso de uso e traduz de outro jeito.
func traduzirErroDeCarteira(err error) error {
	switch {
	case errors.Is(err, wallet.ErrAlreadyExists):
		return apperr.Conflict("o jogador já possui carteira nesta moeda")

	// Indisponibilidade transitória vem ANTES do caso genérico: sem isto, um
	// banco fora do ar cai no default e vira 500, dizendo "há um defeito aqui"
	// para algo que só precisa ser repetido.
	case errors.Is(err, app.ErrUnavailable):
		return apperr.Unavailable("dependência temporariamente indisponível")

	case errors.Is(err, app.ErrNotFound):
		// Não distingue "não existe" de "existe e você não pode ver": a
		// diferença entre as duas respostas é reconhecimento de graça.
		return apperr.NotFound("carteira não encontrada")

	case errors.Is(err, wallet.ErrInvalidID),
		errors.Is(err, wallet.ErrInvalidState):
		return apperr.Malformed(err.Error())

	default:
		// Erro desconhecido vira 500 com mensagem genérica; a causa vai para o
		// log, com o identificador de correlação.
		return apperr.Internal(err)
	}
}

// Ledger devolve uma página do extrato da carteira.
//
//	GET /wallets/:walletId/ledger?cursor=&limit=   (escopo wallets:admin)
func (h *Wallet) Ledger(c *fiber.Ctx) error {
	id, err := identificadorDeCarteira(c)
	if err != nil {
		return err
	}

	var campos contract.Fields

	limite := appwallet.LedgerPageDefault
	if bruto := c.Query("limit"); bruto != "" {
		n, err := strconv.Atoi(bruto)
		switch {
		case err != nil:
			campos.Add("limit", "precisa ser um número inteiro", apperr.FieldCodeInvalidFormat)
		case n <= 0:
			campos.Add("limit", "precisa ser positivo", apperr.FieldCodeInvalidValue)
		case n > appwallet.LedgerPageMax:
			campos.Add("limit",
				fmt.Sprintf("acima do máximo de %d", appwallet.LedgerPageMax),
				apperr.FieldCodeInvalidValue)
		default:
			limite = n
		}
	}

	var depois *appwallet.Position
	if bruto := c.Query("cursor"); bruto != "" {
		p, err := dto.DecodeCursor(bruto)
		if err != nil {
			// Falha fechado. Um cursor corrompido tratado como "começar do
			// início" devolveria a página errada em silêncio, e quem pagina
			// não teria como perceber.
			campos.Add("cursor", "cursor inválido", apperr.FieldCodeInvalidValue)
		} else {
			depois = &p
		}
	}

	if err := campos.Err(); err != nil {
		return err
	}

	pagina, err := h.statement.List(c.UserContext(), id, depois, limite)
	if err != nil {
		return traduzirErroDeCarteira(err)
	}
	return c.Status(http.StatusOK).JSON(dto.NewLedgerPageResponse(id, pagina))
}

// Reconcile confere o saldo contra o ledger. Não altera nada.
//
//	POST /wallets/:walletId/reconciliation   (escopo wallets:admin)
//
// É POST porque o §9 do enunciado assim define, e não porque escreva: a rota é
// uma conferência, e a ausência de escrita tem teste.
func (h *Wallet) Reconcile(c *fiber.Ctx) error {
	id, err := identificadorDeCarteira(c)
	if err != nil {
		return err
	}

	resultado, err := h.reconciler.Run(c.UserContext(), id)
	if err != nil {
		return traduzirErroDeCarteira(err)
	}
	return c.Status(http.StatusOK).JSON(dto.NewReconciliationResponse(resultado))
}

// identificadorDeCarteira lê e valida o identificador do path.
func identificadorDeCarteira(c *fiber.Ctx) (wallet.ID, error) {
	id, err := wallet.ParseID(c.Params("walletId"))
	if err != nil {
		return wallet.ID{}, apperr.Validation(apperr.FieldError{
			Field:   "walletId",
			Message: "identificador de carteira inválido",
			Code:    apperr.FieldCodeInvalidFormat,
		})
	}
	return id, nil
}
