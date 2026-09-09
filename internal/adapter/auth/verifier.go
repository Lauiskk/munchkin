package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

var (
	// ErrTokenExpired distingue o token vencido dos demais inválidos. É
	// informação que o cliente já tem — ele possui o token e pode decodificá-lo
	// — e saber disso o leva a renovar em vez de abrir chamado.
	ErrTokenExpired = errors.New("token expirado")
	// ErrTokenInvalid cobre todo o resto. A causa exata fica no log: dizer ao
	// chamador qual verificação falhou ajuda quem está sondando o serviço.
	ErrTokenInvalid = errors.New("token inválido")
)

// Verifier confere a assinatura e as claims de um access token.
type Verifier struct {
	keys     *KeySet
	issuer   string
	audience string
}

// NewVerifier monta o verificador.
func NewVerifier(keys *KeySet, issuer, audience string) *Verifier {
	return &Verifier{keys: keys, issuer: issuer, audience: audience}
}

// tokenClaims são as claims que a aplicação lê.
type tokenClaims struct {
	jwt.RegisteredClaims
	Scope         string `json:"scope"`
	ProviderID    string `json:"provider_id"`
	AuthorizedTo  string `json:"azp"`
	TokenCategory string `json:"typ"`
}

// Verify valida o token e devolve a identidade que ele representa.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	var claims tokenClaims

	_, err := jwt.ParseWithClaims(raw, &claims,
		func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			if kid == "" {
				return nil, errors.New("cabeçalho do token sem kid")
			}
			return v.keys.KeyFor(ctx, kid)
		},
		// A restrição de algoritmo é a defesa contra confusão de algoritmo:
		// sem ela, um token com alg "none" ou trocado para HS256 usando a
		// chave pública como segredo seria aceito.
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Identity{}, fmt.Errorf("%w: %w", ErrTokenExpired, err)
		}
		return Identity{}, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}

	if claims.Subject == "" {
		return Identity{}, fmt.Errorf("%w: token sem subject", ErrTokenInvalid)
	}

	return Identity{
		Subject:    claims.Subject,
		ClientID:   claims.AuthorizedTo,
		ProviderID: claims.ProviderID,
		scopes:     parseScopes(claims.Scope),
	}, nil
}
