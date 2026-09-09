package middleware

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/pkg/apperr"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

const bearerPrefix = "Bearer "

// TokenVerifier confere um access token e devolve a identidade que ele
// representa. É interface, e não o tipo concreto, para que a cadeia de
// middleware possa ser exercitada sem um IdP de pé — no projeto de referência
// da equipe nada abaixo do handler podia ser testado isoladamente, e é
// exatamente esse acoplamento que se está evitando aqui.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (auth.Identity, error)
}

// LocalsKeyProviderID guarda o provedor autenticado para o log de acesso.
const LocalsKeyProviderID = "providerID"

// Authenticate exige um access token válido em toda rota que não seja pública.
//
// A checagem de rota pública recebe um predicado em vez de uma lista de
// prefixos, e o predicado usa igualdade exata. Com prefixo,
// "/health/live/../../wallets" passaria como rota pública — é uma das formas
// clássicas de furar autorização.
func Authenticate(verifier TokenVerifier, isPublic func(string) bool, log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if isPublic(c.Path()) {
			return c.Next()
		}

		raw, err := bearerToken(c)
		if err != nil {
			return err
		}

		identity, err := verifier.Verify(c.UserContext(), raw)
		if err != nil {
			// O motivo exato fica no log. Devolvê-lo ao chamador ajudaria
			// quem estiver sondando o serviço a descobrir qual verificação
			// contornar. A exceção é o token vencido: o cliente já tem o
			// token e pode decodificá-lo, e saber disso o leva a renovar.
			log.LogAttrs(c.UserContext(), slog.LevelWarn, "auth.rejected",
				slog.String("path", c.Path()),
				slog.String(logs.KeyError, err.Error()))

			if errors.Is(err, auth.ErrTokenExpired) {
				return apperr.Unauthorized("o token expirou")
			}
			return apperr.Unauthorized("credencial inválida")
		}

		c.SetUserContext(auth.IntoContext(c.UserContext(), identity))
		c.Locals(LocalsKeyProviderID, identity.ProviderID)
		return c.Next()
	}
}

// RequireScope exige um escopo específico.
//
// É deliberadamente separado de Authenticate: autenticado não é autorizado, e
// misturar as duas coisas é como se acaba concedendo a um cliente válido uma
// operação que ele não deveria alcançar.
func RequireScope(scope string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		identity, ok := auth.FromContext(c.UserContext())
		if !ok {
			// Chegar aqui sem identidade significa rota protegida registrada
			// fora da cadeia de autenticação. Recusar é o certo: seguir
			// adiante entregaria a operação a um chamador não identificado.
			return apperr.Unauthorized("credencial ausente")
		}
		if !identity.HasScope(scope) {
			return apperr.Forbidden("o token não concede o escopo " + scope)
		}
		return c.Next()
	}
}

// bearerToken extrai o token do cabeçalho Authorization.
func bearerToken(c *fiber.Ctx) (string, error) {
	header := c.Get(fiber.HeaderAuthorization)
	if header == "" {
		return "", apperr.Unauthorized("credencial ausente")
	}
	// A comparação do esquema é sem diferenciar maiúsculas porque a RFC 7235
	// define o esquema como insensível a caixa.
	if len(header) <= len(bearerPrefix) ||
		!strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", apperr.Unauthorized("o cabeçalho Authorization deve usar o esquema Bearer")
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", apperr.Unauthorized("credencial ausente")
	}
	return token, nil
}
