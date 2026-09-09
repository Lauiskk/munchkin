package contract

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

// TypeWagerTransactionRequested é o único tipo de mensagem aceito na entrada.
//
// Recusar tipo desconhecido em vez de ignorar o campo é deliberado: uma
// mensagem com tipo errado que fosse processada como operação financeira
// moveria dinheiro por engano de roteamento.
const TypeWagerTransactionRequested = "WagerTransactionRequested"

// TransactionMessage é o envelope de uma operação recebida por mensageria.
type TransactionMessage struct {
	MessageID  string          `json:"messageId"`
	Type       string          `json:"type"`
	OccurredAt string          `json:"occurredAt"`
	Data       TransactionData `json:"data"`
}

// TransactionData é o corpo da operação, com a chave de idempotência junto.
//
// No HTTP a chave vem em cabeçalho, porque lá ela é metadado de transporte.
// Aqui não há cabeçalho, então ela viaja no corpo — e é a mesma chave, com a
// mesma validação.
type TransactionData struct {
	SubmitTransactionRequest
	IdempotencyKey string `json:"idempotencyKey"`
}

// DecodedMessage é a mensagem já validada.
type DecodedMessage struct {
	MessageID      string
	IdempotencyKey string
	OccurredAt     time.Time
	Transaction    DecodedTransaction
}

// DecodeMessage valida o envelope e o corpo, acumulando os erros por campo.
//
// Recebe os bytes crus, e não a struct, porque o hash da mensagem é sobre o que
// chegou. Calcular o hash sobre uma reserialização faria duas mensagens
// diferentes no fio terem a mesma identidade — e a verificação de reentrega
// deixaria de detectar o que existe para detectar.
func DecodeMessage(corpo []byte) (DecodedMessage, error) {
	var env TransactionMessage
	if err := json.Unmarshal(corpo, &env); err != nil {
		return DecodedMessage{}, apperr.Malformed("o corpo da mensagem não é um JSON válido")
	}

	var campos Fields
	out := DecodedMessage{MessageID: env.MessageID}

	if env.MessageID == "" {
		campos.Add("messageId", "obrigatório", apperr.FieldCodeRequired)
	}
	if env.Type != TypeWagerTransactionRequested {
		campos.Add("type", "tipo de mensagem desconhecido", apperr.FieldCodeInvalidValue)
	}
	if env.OccurredAt != "" {
		if v, err := time.Parse(time.RFC3339, env.OccurredAt); err != nil {
			campos.AddErr("occurredAt", err, apperr.FieldCodeInvalidFormat)
		} else {
			out.OccurredAt = v.UTC()
		}
	}
	if v, err := wagering.ParseIdempotencyKey(env.Data.IdempotencyKey); err != nil {
		campos.AddErr("data.idempotencyKey", err, apperr.FieldCodeInvalidFormat)
	} else {
		out.IdempotencyKey = v
	}

	// A operação em si passa pelo MESMO decode do HTTP. Os erros vêm com os
	// nomes de campo daquele contrato, então recebem o prefixo do envelope.
	operacao, err := env.Data.Decode()
	if err != nil {
		campos.Merge("data.", err)
	} else {
		out.Transaction = operacao
	}

	if err := campos.Err(); err != nil {
		return DecodedMessage{}, err
	}
	return out, nil
}

// HashMessage é a identidade de conteúdo da mensagem.
//
// Serve à verificação de reentrega exigida pelo §10: mesma identidade durável
// com conteúdo diferente é mensagem inválida, não atualização. É um hash
// distinto do de idempotência financeira — este pergunta "é a mesma mensagem?",
// aquele pergunta "é a mesma operação?".
func HashMessage(corpo []byte) []byte {
	soma := sha256.Sum256(corpo)
	return soma[:]
}
