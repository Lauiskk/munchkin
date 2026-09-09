package handler

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/adapter/http/dto"
	"github.com/Lauiskk/munchkin/internal/app"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

// Wallet responde pelas rotas de carteira.
type Wallet struct {
	opener *appwallet.Opener
	getter *appwallet.Getter
}

// NewWallet monta o handler.
func NewWallet(opener *appwallet.Opener, getter *appwallet.Getter) *Wallet {
	return &Wallet{opener: opener, getter: getter}
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
	id, err := wallet.ParseID(c.Params("walletId"))
	if err != nil {
		return apperr.Validation(apperr.FieldError{
			Field:   "walletId",
			Message: "identificador de carteira inválido",
			Code:    apperr.FieldCodeInvalidFormat,
		})
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
