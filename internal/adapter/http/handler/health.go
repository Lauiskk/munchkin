// Package handler reúne os tratadores HTTP.
package handler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/pkg/logs"
)

// ReadinessCheck é uma dependência que precisa responder para o serviço se
// declarar pronto. Cada adaptador registra a sua; o handler não sabe quais são.
type ReadinessCheck interface {
	// Name identifica a dependência na resposta. É público, então não pode
	// conter endereço, credencial ou qualquer detalhe de infraestrutura.
	Name() string
	// Check devolve erro quando a dependência não está utilizável.
	Check(ctx context.Context) error
}

// Health responde pelas sondas de vivacidade e prontidão.
type Health struct {
	log     *slog.Logger
	checks  []ReadinessCheck
	timeout time.Duration
}

// NewHealth monta o handler. O prazo limita o conjunto das verificações: uma
// dependência lenta não pode fazer a sonda pendurar, senão o orquestrador
// interpreta a demora como queda e reinicia um processo saudável.
func NewHealth(log *slog.Logger, timeout time.Duration, checks ...ReadinessCheck) *Health {
	return &Health{log: log, checks: checks, timeout: timeout}
}

type liveBody struct {
	Status string `json:"status"`
}

type readyBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

const (
	statusAlive    = "alive"
	statusReady    = "ready"
	statusNotReady = "not_ready"
	statusOK       = "ok"
	statusFailing  = "failing"
)

// Live responde pela vivacidade do processo.
//
// Não consulta dependência nenhuma, e isso é o ponto: se a sonda de vivacidade
// falhasse por causa do banco, o orquestrador reiniciaria a aplicação inteira
// durante uma indisponibilidade do banco — trocando um problema por dois.
// Quem responde por dependência é a prontidão.
func (h *Health) Live(c *fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(liveBody{Status: statusAlive})
}

// Ready responde pela prontidão para receber tráfego.
//
// A resposta diz qual dependência está falhando, mas nunca por quê: a mensagem
// de erro carregaria host, usuário e às vezes credencial, e este endpoint é
// público. O motivo vai para o log.
func (h *Health) Ready(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.UserContext(), h.timeout)
	defer cancel()

	results := make(map[string]string, len(h.checks))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, check := range h.checks {
		wg.Add(1)
		go func(check ReadinessCheck) {
			defer wg.Done()
			status := statusOK
			if err := check.Check(ctx); err != nil {
				status = statusFailing
				h.log.LogAttrs(ctx, slog.LevelWarn, "health.dependency_failing",
					slog.String("dependency", check.Name()),
					slog.String(logs.KeyError, err.Error()))
			}
			mu.Lock()
			results[check.Name()] = status
			mu.Unlock()
		}(check)
	}
	wg.Wait()

	body := readyBody{Status: statusReady, Checks: results}
	code := fiber.StatusOK
	for _, status := range results {
		if status != statusOK {
			body.Status = statusNotReady
			code = fiber.StatusServiceUnavailable
			break
		}
	}
	return c.Status(code).JSON(body)
}
