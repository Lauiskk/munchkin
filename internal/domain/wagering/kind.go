package wagering

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

var (
	// ErrInvalidKind indica tipo de operação desconhecido.
	ErrInvalidKind = errors.New("tipo de operação inválido")
	// ErrInvalidStatus indica estado desconhecido.
	ErrInvalidStatus = errors.New("estado de transação inválido")
	// ErrInvalidTransition indica transição não permitida pela máquina de estados.
	ErrInvalidTransition = errors.New("transição de estado não permitida")
	// ErrTerminal indica tentativa de transicionar a partir de estado terminal.
	ErrTerminal = errors.New("transação em estado terminal não sofre nova transição")
)

// Kind é o tipo da operação.
type Kind string

const (
	// Opening é a abertura interna de carteira. Reservado: rejeitado quando
	// chega por HTTP ou por fila.
	Opening  Kind = "OPENING"
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

var kindsValidos = map[Kind]struct{}{
	Opening: {}, Bet: {}, Win: {}, Loss: {}, Refund: {}, Rollback: {},
}

// ParseKind valida o tipo.
func ParseKind(s string) (Kind, error) {
	k := Kind(s)
	if _, ok := kindsValidos[k]; !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidKind, truncar(s))
	}
	return k, nil
}

// ParseExternalKind valida o tipo aceitando apenas origens externas.
//
// OPENING é recusado aqui: é a abertura interna de carteira, e um provedor que
// pudesse enviá-la creditaria a própria carteira sem passar pelo caminho de
// abertura.
func ParseExternalKind(s string) (Kind, error) {
	k, err := ParseKind(s)
	if err != nil {
		return "", err
	}
	if k == Opening {
		return "", fmt.Errorf("%w: %s é reservado à abertura interna de carteira",
			ErrInvalidKind, Opening)
	}
	return k, nil
}

// IsInternal informa se o tipo é de origem interna.
func (k Kind) IsInternal() bool { return k == Opening }

// IsReversal informa se o tipo desfaz outra operação.
func (k Kind) IsReversal() bool { return k == Refund || k == Rollback }

// reversalTargets declara o que cada reversão pode desfazer.
//
// Em dado, e não em condicionais, pelo mesmo motivo da máquina de estados: a
// regra inteira é legível de uma vez, e não existe combinação permitida que
// ninguém saiba que existe.
//
// REFUND devolve uma aposta; ROLLBACK desfaz qualquer movimentação que tenha
// acontecido. LOSS não aparece em lugar nenhum porque não movimentou nada — não
// há o que desfazer. E ROLLBACK não desfaz ROLLBACK: seria refazer.
var reversalTargets = map[Kind]map[Kind]struct{}{
	Refund:   {Bet: {}},
	Rollback: {Bet: {}, Win: {}, Refund: {}},
}

// CanReverse informa se este tipo de reversão pode desfazer a operação indicada.
func (k Kind) CanReverse(alvo Kind) bool {
	alvos, ok := reversalTargets[k]
	if !ok {
		return false
	}
	_, permitido := alvos[alvo]
	return permitido
}

// CreditsWallet informa se o tipo credita a carteira quando processado.
//
// É o que define a direção da reversão: o movimento contrário ao original. Uma
// aposta debitou, então revertê-la credita; um ganho creditou, então revertê-lo
// debita.
func (k Kind) CreditsWallet() bool { return k == Win || k == Refund }

// RequiresZeroAmount informa se o tipo exige valor exatamente zero.
//
// LOSS registra a perda de uma rodada sem movimentar a carteira: o dinheiro já
// saiu na aposta. Exigir zero impede que uma perda debite de novo.
func (k Kind) RequiresZeroAmount() bool { return k == Loss }

func (k Kind) String() string { return string(k) }

// Status é o estado da transação.
type Status string

const (
	// Pending é o estado inicial, dentro da transação SQL.
	Pending Status = "PENDING"
	// PendingReference indica espera por uma referência ainda não recebida.
	PendingReference Status = "PENDING_REFERENCE"
	// Processed é conclusão com sucesso. Terminal.
	Processed Status = "PROCESSED"
	// Rejected é recusa por regra de negócio. Terminal.
	Rejected Status = "REJECTED"
	// Failed é falha permanente de infraestrutura, registrada para auditoria.
	// Terminal.
	Failed Status = "FAILED"
)

// transicoes declara a máquina de estados. Um estado ausente do mapa, ou com
// conjunto vazio, é terminal.
//
// Declarar as transições em dado, e não espalhadas em condicionais, torna a
// máquina inteira legível de uma vez — e torna impossível existir uma transição
// que ninguém sabe que existe.
var transicoes = map[Status]map[Status]struct{}{
	Pending: {
		PendingReference: {},
		Processed:        {},
		Rejected:         {},
		Failed:           {},
	},
	PendingReference: {
		Processed: {},
		Rejected:  {},
		Failed:    {},
	},
	Processed: {},
	Rejected:  {},
	Failed:    {},
}

var statusValidos = map[Status]struct{}{
	Pending: {}, PendingReference: {}, Processed: {}, Rejected: {}, Failed: {},
}

// ParseStatus valida o estado.
func ParseStatus(s string) (Status, error) {
	st := Status(s)
	if _, ok := statusValidos[st]; !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidStatus, truncar(s))
	}
	return st, nil
}

// IsTerminal informa se o estado não admite nova transição.
func (s Status) IsTerminal() bool {
	destinos, conhecido := transicoes[s]
	return conhecido && len(destinos) == 0
}

// CanTransitionTo informa se a transição é permitida.
func (s Status) CanTransitionTo(destino Status) bool {
	destinos, ok := transicoes[s]
	if !ok {
		return false
	}
	_, permitido := destinos[destino]
	return permitido
}

func (s Status) String() string { return string(s) }

// FailureCode é o motivo estável de uma recusa.
//
// É contrato: uma vez publicado, o código não muda de significado. O provedor
// decide o que fazer com base nele, e mudar o sentido de um código quebraria
// integração alheia em silêncio.
type FailureCode string

const (
	// FailureInsufficientFunds — a aposta excede o saldo do jogador.
	FailureInsufficientFunds FailureCode = "INSUFFICIENT_FUNDS"
	// FailureReversalInsufficientFunds — a reversão precisaria debitar mais que
	// o saldo disponível. Distinto do anterior de propósito: o enunciado exige
	// que sejam diferentes, porque para quem audita são situações distintas.
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	// FailureReferenceNotFound — a referência não chegou dentro do prazo.
	FailureReferenceNotFound FailureCode = "REFERENCE_NOT_FOUND"
	// FailureReferenceNotProcessed — a referência existe mas terminou sem
	// sucesso; não há o que reverter.
	FailureReferenceNotProcessed FailureCode = "REFERENCE_NOT_PROCESSED"
	// FailureReferenceAlreadyReversed — a referência já foi revertida.
	FailureReferenceAlreadyReversed FailureCode = "REFERENCE_ALREADY_REVERSED"
	// FailureReferenceMismatch — a referência diverge em provedor, jogador,
	// carteira, moeda ou rodada.
	FailureReferenceMismatch FailureCode = "REFERENCE_MISMATCH"
	// FailureAmountMismatch — o valor da reversão difere do referenciado.
	FailureAmountMismatch FailureCode = "AMOUNT_MISMATCH"
	// FailureCurrencyMismatch — a moeda diverge da carteira.
	FailureCurrencyMismatch FailureCode = "CURRENCY_MISMATCH"
	// FailureWalletNotFound — a carteira indicada não existe.
	FailureWalletNotFound FailureCode = "WALLET_NOT_FOUND"
	// FailureWalletPlayerMismatch — a carteira não pertence ao jogador.
	FailureWalletPlayerMismatch FailureCode = "WALLET_PLAYER_MISMATCH"
	// FailureInvalidAmountForKind — o valor não é o exigido pelo tipo.
	FailureInvalidAmountForKind FailureCode = "INVALID_AMOUNT_FOR_KIND"
	// FailureBalanceLimitExceeded — o crédito levaria o saldo além do maior
	// valor representável. A entrada é válida e a carteira existe; é o
	// RESULTADO que não cabe, e isso é recusa de negócio, não defeito.
	FailureBalanceLimitExceeded FailureCode = "BALANCE_LIMIT_EXCEEDED"
	// FailureInternalError — falha permanente de infraestrutura.
	FailureInternalError FailureCode = "INTERNAL_ERROR"
)

var failureCodesValidos = map[FailureCode]struct{}{
	FailureInsufficientFunds: {}, FailureReversalInsufficientFunds: {},
	FailureReferenceNotFound: {}, FailureReferenceNotProcessed: {},
	FailureReferenceAlreadyReversed: {}, FailureReferenceMismatch: {},
	FailureAmountMismatch: {}, FailureCurrencyMismatch: {},
	FailureWalletNotFound: {}, FailureWalletPlayerMismatch: {},
	FailureInvalidAmountForKind: {}, FailureBalanceLimitExceeded: {},
	FailureInternalError: {},
}

// ParseFailureCode valida o código de falha.
func ParseFailureCode(s string) (FailureCode, error) {
	c := FailureCode(s)
	if _, ok := failureCodesValidos[c]; !ok {
		return "", fmt.Errorf("código de falha desconhecido: %q", truncar(s))
	}
	return c, nil
}

func (f FailureCode) String() string { return string(f) }

// truncar limita o texto ecoado em mensagem de erro.
func truncar(s string) string {
	const limite = 48
	if len(s) <= limite {
		return s
	}
	// O corte respeita a fronteira do caractere. Cortar por byte parte um
	// caractere multibyte ao meio e produz UTF-8 inválido — que o PostgreSQL
	// recusa numa coluna TEXT e o SQS recusa num atributo de mensagem. A
	// entrada aqui vem de fora, então acento no lugar errado é questão de
	// tempo, não de hipótese.
	corte := limite
	for corte > 0 && !utf8.RuneStart(s[corte]) {
		corte--
	}
	return s[:corte] + "…"
}
