// Package logs monta o logger estruturado da aplicação.
//
// O formato padrão é JSON no stdout, seguindo a orientação de que o processo
// escreve e o ambiente coleta. O enunciado exige que o log carregue os
// identificadores disponíveis da operação e que não registre credencial, dado
// sensível ou payload financeiro completo — por isso o log carrega
// identificador, nunca conteúdo.
package logs

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"

	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/correlation"
)

// Chaves canônicas. Usar constante em vez de literal evita que a mesma
// informação apareça como "walletId" num arquivo e "wallet_id" noutro, o que
// inutilizaria a busca justamente quando ela é mais necessária.
const (
	KeyCorrelationID = "correlationId"
	KeyMessageID     = "messageId"
	KeyTransactionID = "transactionId"
	KeyWalletID      = "walletId"
	KeyProviderID    = "providerId"
	KeyEventID       = "eventId"
	KeyEventType     = "eventType"
	KeyAggregateID   = "aggregateId"
	KeyTraceID       = "traceId"
	KeyError         = "error"
)

// New monta o logger a partir da configuração.
func New(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level(cfg.Log.Level)}

	var h slog.Handler
	if cfg.Log.Format == config.LogFormatText {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(&contextHandler{Handler: h})
}

func level(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// contextHandler injeta no registro o que está no context, para que quem loga
// não precise repassar o identificador de correlação em toda chamada — repassar
// à mão significa esquecer em algum lugar, e o log fica cego justamente na
// linha que interessa.
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := correlation.From(ctx); id != "" {
		r.AddAttrs(slog.String(KeyCorrelationID, id))
	}
	// O identificador do trace entra em TODA linha, e não só na do fim da
	// requisição. É o que liga um log a um trace: sem ele, quem tem o trace não
	// acha a linha correspondente, e quem tem a linha não acha o trace — e as
	// duas ferramentas ficam sendo duas, em vez de uma.
	//
	// Com o tracing desligado o span é nulo, o contexto não é válido, e nada é
	// acrescentado.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String(KeyTraceID, sc.TraceID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
