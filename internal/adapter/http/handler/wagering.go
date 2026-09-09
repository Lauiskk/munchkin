package handler

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/internal/adapter/http/dto"
	"github.com/Lauiskk/munchkin/internal/app"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

// HeaderIdempotencyKey é o cabeçalho obrigatório na submissão de operação.
const HeaderIdempotencyKey = "Idempotency-Key"

// Wagering responde pelas rotas de operação financeira.
type Wagering struct {
	processor *appwagering.Processor
	querier   *appwagering.Querier
}

// NewWagering monta o handler.
func NewWagering(p *appwagering.Processor, q *appwagering.Querier) *Wagering {
	return &Wagering{processor: p, querier: q}
}

// Submit recebe uma operação financeira.
//
//	POST /wagering/transactions   (escopo wagering:write)
func (h *Wagering) Submit(c *fiber.Ctx) error {
	chave, err := wagering.ParseIdempotencyKey(c.Get(HeaderIdempotencyKey))
	if err != nil {
		return apperr.Validation(apperr.FieldError{
			Field:   HeaderIdempotencyKey,
			Message: err.Error(),
			Code:    apperr.FieldCodeRequired,
		})
	}

	var req dto.SubmitTransactionRequest
	if err := dto.Bind(c, &req); err != nil {
		return err
	}
	decodificado, err := req.Decode()
	if err != nil {
		return err
	}

	identidade, ok := auth.FromContext(c.UserContext())
	if !ok {
		return apperr.Unauthorized("credencial ausente")
	}

	// O provedor da operação é o do TOKEN. O corpo que discordar é recusado com
	// 403, não 400: não é entrada malformada, é tentativa de agir como outro
	// provedor — e a distinção importa para quem lê o log.
	if string(decodificado.ProviderID) != identidade.ProviderID {
		return apperr.Forbidden("o providerId do corpo não corresponde ao token")
	}

	out, err := h.processor.Process(c.UserContext(), appwagering.Input{
		ProviderID:          decodificado.ProviderID,
		ExternalID:          decodificado.ExternalID,
		IdempotencyKey:      chave,
		PlayerID:            decodificado.PlayerID,
		WalletID:            decodificado.WalletID,
		RoundID:             decodificado.RoundID,
		GameID:              decodificado.GameID,
		Kind:                decodificado.Kind,
		Money:               decodificado.Money,
		ReferenceExternalID: decodificado.ReferenceExternalID,
	})
	if err != nil {
		return traduzirErroDeOperacao(err)
	}

	return responderOperacao(c, out)
}

// responderOperacao escolhe o código de resposta a partir do desfecho.
//
// Recusa de negócio é 422 com o código de falha, distinta de 400 (entrada
// inválida) e de 409 (conflito de idempotência). O enunciado exige que essas
// situações sejam distinguíveis pelo contrato.
func responderOperacao(c *fiber.Ctx, out appwagering.Output) error {
	corpo := dto.TransactionResponse{
		Status:           out.Status,
		FailureCode:      out.FailureCode,
		IdempotentReplay: out.IdempotentReplay,
	}
	if !out.TransactionID.IsZero() {
		id := out.TransactionID
		corpo.TransactionID = &id
	}
	if out.Balance.IsValid() {
		saldo := out.Balance
		corpo.Balance = &saldo
	}

	switch out.Status {
	case wagering.Rejected:
		return c.Status(http.StatusUnprocessableEntity).JSON(corpo)
	case wagering.PendingReference:
		// 202: aceita, ainda não concluída. Distinguir do 200 importa — o
		// provedor precisa saber que ainda não há desfecho, e o enunciado exige
		// que processamento pendente seja distinguível pelo contrato.
		return c.Status(http.StatusAccepted).JSON(corpo)
	default:
		return c.Status(http.StatusOK).JSON(corpo)
	}
}

// GetByID devolve uma operação pela identidade interna.
//
//	GET /wagering/transactions/:transactionId   (escopo wagering:read)
func (h *Wagering) GetByID(c *fiber.Ctx) error {
	id, err := wagering.ParseTransactionID(c.Params("transactionId"))
	if err != nil {
		return apperr.Validation(apperr.FieldError{
			Field:   "transactionId",
			Message: "identificador de transação inválido",
			Code:    apperr.FieldCodeInvalidFormat,
		})
	}

	identidade, ok := auth.FromContext(c.UserContext())
	if !ok {
		return apperr.Unauthorized("credencial ausente")
	}

	t, err := h.querier.ByID(c.UserContext(), id, wagering.ProviderID(identidade.ProviderID))
	if err != nil {
		return traduzirErroDeOperacao(err)
	}
	return c.Status(http.StatusOK).JSON(dto.NewTransactionDetail(t))
}

// GetByExternalID devolve uma operação pela identidade no provedor.
//
//	GET /providers/:providerId/wagering/transactions/:externalTransactionId
//
// O provedor do caminho é confrontado com o do token. Usar o do caminho na busca
// permitiria a qualquer provedor consultar transações de outro trocando um
// segmento do endereço.
func (h *Wagering) GetByExternalID(c *fiber.Ctx) error {
	identidade, ok := auth.FromContext(c.UserContext())
	if !ok {
		return apperr.Unauthorized("credencial ausente")
	}

	if c.Params("providerId") != identidade.ProviderID {
		// 404, e não 403: um 403 confirmaria que o outro provedor existe.
		return apperr.NotFound("transação não encontrada")
	}

	external, err := wagering.ParseExternalID(c.Params("externalTransactionId"))
	if err != nil {
		return apperr.Validation(apperr.FieldError{
			Field:   "externalTransactionId",
			Message: err.Error(),
			Code:    apperr.FieldCodeInvalidFormat,
		})
	}

	t, err := h.querier.ByExternalID(c.UserContext(),
		wagering.ProviderID(identidade.ProviderID), external)
	if err != nil {
		return traduzirErroDeOperacao(err)
	}
	return c.Status(http.StatusOK).JSON(dto.NewTransactionDetail(t))
}

func traduzirErroDeOperacao(err error) error {
	switch {
	case errors.Is(err, appwagering.ErrIdempotencyConflict):
		return apperr.Conflict(err.Error())
	case errors.Is(err, app.ErrNotFound):
		return apperr.NotFound("transação não encontrada")
	default:
		return apperr.Internal(err)
	}
}
