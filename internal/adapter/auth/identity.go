package auth

import (
	"context"
	"strings"
)

// Escopos reconhecidos pela aplicação.
const (
	ScopeWageringWrite = "wagering:write"
	ScopeWageringRead  = "wagering:read"
	ScopeWalletsAdmin  = "wallets:admin"
)

// Identity é quem está chamando, derivado exclusivamente do token.
//
// Nenhum campo daqui pode ser sobrescrito por corpo, query ou parâmetro de
// rota. É essa regra que sustenta o isolamento entre provedores: o provedor da
// operação é o do token, e um corpo que discorde é recusado.
type Identity struct {
	// Subject é o `sub` — a conta de serviço do cliente.
	Subject string
	// ClientID é o `azp` — qual cliente do IdP obteve este token.
	ClientID string
	// ProviderID vem da claim `provider_id`. Vazio para o serviço interno,
	// que não é um provedor e não deve poder agir como se fosse.
	ProviderID string

	scopes map[string]struct{}
}

// NewIdentity monta uma identidade. Existe para os testes e para o dia em que
// houver outra origem de identidade além do token JWT.
func NewIdentity(subject, clientID, providerID string, scopes ...string) Identity {
	set := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		set[s] = struct{}{}
	}
	return Identity{
		Subject:    subject,
		ClientID:   clientID,
		ProviderID: providerID,
		scopes:     set,
	}
}

// HasScope informa se o token concede o escopo.
func (i Identity) HasScope(scope string) bool {
	_, ok := i.scopes[scope]
	return ok
}

// IsProvider informa se a identidade representa um provedor externo.
func (i Identity) IsProvider() bool { return i.ProviderID != "" }

// Scopes devolve os escopos concedidos, para log e diagnóstico.
func (i Identity) Scopes() []string {
	out := make([]string, 0, len(i.scopes))
	for s := range i.scopes {
		out = append(out, s)
	}
	return out
}

// parseScopes lê a claim `scope`, que o OAuth define como lista separada por
// espaço.
func parseScopes(raw string) map[string]struct{} {
	fields := strings.Fields(raw)
	scopes := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		scopes[f] = struct{}{}
	}
	return scopes
}

type identityCtxKey struct{}

// IntoContext devolve um context carregando a identidade.
func IntoContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// FromContext extrai a identidade. O segundo retorno é falso em rota pública,
// onde não há identidade — e nunca deve ser ignorado num caminho autenticado.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}
