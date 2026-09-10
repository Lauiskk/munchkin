package middleware

import (
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Lauiskk/munchkin/pkg/correlation"
)

// portadorFiber adapta os cabeçalhos do Fiber ao contrato de propagação do
// OpenTelemetry.
//
// Existe porque o Fiber roda sobre fasthttp, que não é `net/http` — a
// instrumentação pronta do ecossistema não serve, e a alternativa seria copiar
// cabeçalhos para um `http.Header` a cada requisição só para satisfazer uma
// interface.
type portadorFiber struct{ c *fiber.Ctx }

func (p portadorFiber) Get(chave string) string { return p.c.Get(chave) }
func (p portadorFiber) Set(chave, valor string) { p.c.Set(chave, valor) }

func (p portadorFiber) Keys() []string {
	var chaves []string
	p.c.Request().Header.VisitAll(func(k, _ []byte) {
		chaves = append(chaves, string(k))
	})
	return chaves
}

// Tracing abre um span por requisição, continuando o trace de quem chamou.
//
// Com o tracing desligado o tracer é nulo: `Start` devolve um span que não
// registra nada e não aloca. Por isso o middleware pode ficar sempre na cadeia,
// sem um `if` que teria de ser mantido correto.
func Tracing(tracer trace.Tracer, propagador propagation.TextMapPropagator) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// O `traceparent` que chega é dado externo, mas decide apenas o
		// identificador do trace — não autorização nem roteamento. Um valor
		// forjado polui o próprio trace de quem o forjou, e nada mais.
		ctx := propagador.Extract(c.UserContext(), portadorFiber{c})

		// O nome provisório usa o caminho cru porque a ROTA ainda não foi
		// resolvida: este middleware roda antes do roteamento, e c.Route()
		// devolveria vazio. O nome definitivo é ajustado depois do handler.
		ctx, span := tracer.Start(ctx, c.Method()+" "+c.Path(),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Method()),
				attribute.String("url.path", c.Path()),
				// O identificador de correlação vai NO span, e o do trace vai
				// no log: sem os dois se apontarem, quem tem um não acha o
				// outro.
				attribute.String("correlation.id", correlation.From(c.UserContext())),
			))
		defer span.End()

		c.SetUserContext(ctx)
		err := c.Next()

		// Agora a rota existe. O span passa a se chamar pelo TEMPLATE — "GET
		// /wallets/{walletId}" e não "GET /wallets/01a0...". Sem isso cada
		// carteira viraria uma operação distinta no Jaeger, e agrupar latência
		// por rota deixaria de ser possível.
		if rota := c.Route().Path; rota != "" {
			span.SetName(c.Method() + " " + rota)
			span.SetAttributes(attribute.String("http.route", rota))
		}
		span.SetAttributes(attribute.Int("http.response.status_code", c.Response().StatusCode()))
		if provedor := c.Locals(LocalsKeyProviderID); provedor != nil {
			if s, ok := provedor.(string); ok && s != "" {
				span.SetAttributes(attribute.String("provider.id", s))
			}
		}
		return err
	}
}
