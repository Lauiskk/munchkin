package wagering

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

var (
	// ErrInvalidIdempotencyKey indica chave ausente ou fora do formato.
	ErrInvalidIdempotencyKey = errors.New("chave de idempotência inválida")
)

// maxIdempotencyKeyLen limita a chave recebida do provedor.
//
// Ela entra em índice único e em log. Sem limite, é o provedor quem decide o
// tamanho do que gravamos e registramos.
const maxIdempotencyKeyLen = 128

// ParseIdempotencyKey valida a chave de idempotência.
//
// A chave NÃO entra no hash — ela identifica a tentativa, não a operação —, mas
// é validada porque vira parte de um índice e aparece em log.
//
// O servidor não substitui a chave recebida por uma calculada. O cliente pode
// construí-la como "{provedor}:{idExterno}", e a maioria constrói, mas trocá-la
// em silêncio faria o servidor deduplicar por um critério diferente do que o
// cliente acredita estar usando.
func ParseIdempotencyKey(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: ausente", ErrInvalidIdempotencyKey)
	}
	if len(s) > maxIdempotencyKeyLen {
		return "", fmt.Errorf("%w: %d caracteres excedem o máximo de %d",
			ErrInvalidIdempotencyKey, len(s), maxIdempotencyKeyLen)
	}
	if strings.TrimSpace(s) != s {
		return "", fmt.Errorf("%w: não pode ter espaço nas bordas", ErrInvalidIdempotencyKey)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == ':', r == '.':
		default:
			return "", fmt.Errorf("%w: caractere não permitido (%q)",
				ErrInvalidIdempotencyKey, string(r))
		}
	}
	return s, nil
}

// Fingerprint são os campos de negócio que identificam uma operação.
//
// É deliberadamente uma struct própria, e não a transação inteira: o hash tem de
// cobrir o que define a operação, e nada mais. Instante de recebimento,
// identificador interno, estado e tentativas mudam entre um envio e o reenvio da
// MESMA operação — incluí-los faria todo reenvio parecer conteúdo diferente.
type Fingerprint struct {
	ProviderID          ProviderID
	ExternalID          ExternalID
	PlayerID            wallet.PlayerID
	WalletID            wallet.ID
	RoundID             RoundID
	GameID              GameID
	Kind                Kind
	Amount              money.Money
	ReferenceExternalID ExternalID
}

// campos monta a representação canônica.
//
// O conjunto de chaves é FIXO: a referência aparece sempre, vazia quando não se
// aplica. Incluí-la condicionalmente faria o mesmo conteúdo produzir hashes
// diferentes conforme o campo estivesse presente ou ausente no JSON recebido —
// e o cliente não controla isso de forma confiável.
func (f Fingerprint) campos() map[string]string {
	return map[string]string{
		"providerId":            f.ProviderID.String(),
		"externalTransactionId": f.ExternalID.String(),
		"playerId":              f.PlayerID.String(),
		"walletId":              f.WalletID.String(),
		"roundId":               f.RoundID.String(),
		"gameId":                f.GameID.String(),
		"kind":                  f.Kind.String(),
		// O valor é a forma canônica produzida pelo tipo, não o texto cru
		// recebido. Como o tipo aceita uma grafia por valor, a normalização é a
		// própria recusa do que não é canônico — não há duas grafias que
		// cheguem até aqui e produzam hashes diferentes.
		"money.amount":                   f.Amount.Amount(),
		"money.currency":                 f.Amount.Currency().String(),
		"referenceExternalTransactionId": f.ReferenceExternalID.String(),
	}
}

// Hash devolve o SHA-256 do JSON canônico dos campos de negócio.
//
// A ordenação das chaves vem do encoding/json, que serializa mapas com as
// chaves ordenadas — propriedade documentada da biblioteca padrão, não
// coincidência de implementação. Duas requisições com os mesmos campos em
// ordens diferentes no corpo produzem o mesmo hash, que é o ponto.
//
// A entrada por HTTP e a entrada por fila constroem o MESMO Fingerprint, então
// produzem o mesmo hash para o mesmo negócio — é o que torna as duas portas
// equivalentes para a idempotência.
func (f Fingerprint) Hash() ([]byte, error) {
	if !f.Amount.IsValid() {
		return nil, fmt.Errorf("%w: valor ausente no cálculo do hash", money.ErrUninitialized)
	}

	canonico, err := json.Marshal(f.campos())
	if err != nil {
		return nil, fmt.Errorf("serialização canônica: %w", err)
	}

	soma := sha256.Sum256(canonico)
	return soma[:], nil
}

// Canonical devolve a representação canônica em texto, para diagnóstico.
//
// Existe para que uma divergência de hash possa ser investigada comparando o que
// de fato entrou no cálculo, em vez de dois hexadecimais que não dizem nada.
func (f Fingerprint) Canonical() string {
	canonico, err := json.Marshal(f.campos())
	if err != nil {
		return ""
	}
	return string(canonico)
}
