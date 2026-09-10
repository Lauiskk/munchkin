// Package tracing liga o OpenTelemetry — quando pedido.
//
// Desligado é o padrão, e não uma conveniência de desenvolvimento. Um
// exportador ativo por engano manda dados de operação para um endereço
// configurado em algum lugar; o padrão seguro é não mandar. Desligado, o
// provedor é nulo: sem exportador, sem goroutine de envio, sem span alocado por
// requisição.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/config"
)

// Nome do instrumentador. Aparece no trace como a biblioteca que criou o span.
const nome = "github.com/Lauiskk/munchkin"

// exportTimeout limita a descarga de spans no encerramento.
//
// Curto de propósito: trace é diagnóstico, e um coletor lento não pode segurar
// o encerramento de um serviço que move dinheiro.
const exportTimeout = 5 * time.Second

// Tracer é o que os adaptadores usam para abrir spans.
type Tracer struct{ trace.Tracer }

// Novo monta o provedor. Devolve um tracer nulo quando o tracing está desligado.
func Novo(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*Tracer, error) {
	// A propagação é registrada SEMPRE, mesmo desligada: ela só lê e escreve
	// cabeçalhos, e mantê-la ativa faz um `traceparent` que atravessa o serviço
	// continuar válido para quem estiver do outro lado.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if !cfg.Tracing.Enabled {
		otel.SetTracerProvider(noop.NewTracerProvider())
		log.Info("tracing.disabled")
		return &Tracer{noop.NewTracerProvider().Tracer(nome)}, nil
	}

	exportador, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(cfg.Tracing.Endpoint),
		otlptracehttp.WithTimeout(exportTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("exportador de trace: %w", err)
	}

	// A versão do semconv tem de ser a MESMA que o `resource.Default()` usa: o
	// Merge recusa esquemas conflitantes, e o erro só aparece na subida — que é
	// onde ele deve aparecer, mas custa um ciclo se a versão for escolhida no
	// chute em vez de conferida contra o SDK.
	recursos, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.Tracing.ServiceName),
		attribute.String("deployment.environment", cfg.App.Env),
	))
	if err != nil {
		return nil, fmt.Errorf("recurso de trace: %w", err)
	}

	provedor := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exportador, sdktrace.WithExportTimeout(exportTimeout)),
		sdktrace.WithResource(recursos),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.Tracing.SampleRatio))),
	)
	otel.SetTracerProvider(provedor)

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, exportTimeout)
			defer cancel()
			// Falha ao descarregar é registrada, não propagada: perder trace é
			// perder diagnóstico, e derrubar o encerramento por isso trocaria
			// um problema pequeno por um grande.
			if err := provedor.Shutdown(ctx); err != nil {
				log.Warn("tracing.shutdown_failed", slog.String("error", err.Error()))
			}
			return nil
		},
	})

	log.Info("tracing.enabled",
		slog.String("endpoint", cfg.Tracing.Endpoint),
		slog.Float64("sample_ratio", cfg.Tracing.SampleRatio))

	return &Tracer{provedor.Tracer(nome)}, nil
}

// Module provê o tracer.
var Module = fx.Module("tracing", fx.Provide(Novo))

// Nulo devolve um tracer que não registra nada.
//
// É o mesmo que a aplicação usa com o tracing desligado, e existe exportado
// para que teste e composição parcial não precisem montar um provedor — nem
// carregar um ponteiro nulo esperando para estourar.
func Nulo() *Tracer { return &Tracer{noop.NewTracerProvider().Tracer(nome)} }
