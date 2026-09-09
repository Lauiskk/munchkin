//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/app/outbox"
	"github.com/Lauiskk/munchkin/internal/domain/event"
)

// publicadorFalso registra o que sairia para o transporte.
//
// O transporte real é coberto pelos testes com LocalStack: aqui o que está sob
// prova é a coordenação no banco — reivindicação, lease, backoff e confirmação
// —, e um SQS de verdade só acrescentaria latência a isso.
type publicadorFalso struct {
	mu        sync.Mutex
	enviadas  []outbox.Message
	erro      error
	falharPor map[string]error
}

func (p *publicadorFalso) Publish(_ context.Context, m outbox.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.erro != nil {
		return p.erro
	}
	if err, ok := p.falharPor[m.DeduplicationID]; ok {
		return err
	}
	p.enviadas = append(p.enviadas, m)
	return nil
}

func (p *publicadorFalso) recebidas() []outbox.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]outbox.Message(nil), p.enviadas...)
}

// repoQueFalhaAoConfirmar simula a queda ENTRE publicar e confirmar.
type repoQueFalhaAoConfirmar struct {
	outbox.Repository
	falhar bool
}

func (r *repoQueFalhaAoConfirmar) MarkPublished(ctx context.Context, id event.ID) error {
	if r.falhar {
		return errors.New("processo caiu antes de confirmar")
	}
	return r.Repository.MarkPublished(ctx, id)
}

type ambienteOutbox struct {
	ambienteOperacao
	repo       *postgres.OutboxRepository
	publicador *publicadorFalso
	dispatcher *outbox.Dispatcher
}

func novoAmbienteOutbox(t *testing.T) ambienteOutbox {
	t.Helper()
	a := novoAmbienteOperacao(t)
	repo := postgres.NewOutboxRepository(a.db)
	pub := &publicadorFalso{}

	return ambienteOutbox{
		ambienteOperacao: a,
		repo:             repo,
		publicador:       pub,
		dispatcher: outbox.NewDispatcher(repo, pub,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			"instancia-de-teste", 50, time.Minute),
	}
}

// executar roda uma instrução de manutenção direto no banco.
func (a ambienteOutbox) executar(t *testing.T, sql string, args ...any) {
	t.Helper()
	require.NoError(t, a.db.Session(a.ctx).Exec(sql, args...).Error)
}

// AC-1
func TestEventoPendenteEPublicadoEConfirmado(t *testing.T) {
	a := novoAmbienteOutbox(t)
	a.carteira(t, "100.00") // abertura grava dois eventos na outbox

	publicados, err := a.dispatcher.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Equal(t, 2, publicados)
	assert.Len(t, a.publicador.recebidas(), 2)
	assert.Zero(t, a.conta(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`),
		"nenhum evento pode ficar pendente depois de publicado")
	assert.Zero(t, a.conta(t, `SELECT count(*) FROM outbox_events WHERE locked_by IS NOT NULL`),
		"a confirmação libera o lease")
}

// AC-2
func TestEventoJaPublicadoNaoSaiDeNovo(t *testing.T) {
	a := novoAmbienteOutbox(t)
	a.carteira(t, "100.00")

	_, err := a.dispatcher.RunOnce(a.ctx)
	require.NoError(t, err)

	publicados, err := a.dispatcher.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Zero(t, publicados)
	assert.Len(t, a.publicador.recebidas(), 2, "a segunda rodada não pode reenviar nada")
}

// AC-3
func TestFalhaNaPublicacaoContaTentativaEDevolveOEvento(t *testing.T) {
	a := novoAmbienteOutbox(t)
	a.carteira(t, "100.00")
	a.publicador.erro = errors.New("fila indisponível")

	publicados, err := a.dispatcher.RunOnce(a.ctx)

	require.NoError(t, err, "falha de transporte não é erro da rodada")
	assert.Zero(t, publicados)
	assert.Equal(t, int64(2), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE published_at IS NULL AND attempts = 1
		  AND last_error IS NOT NULL
		  AND next_attempt_at > now()
		  AND locked_by IS NULL AND locked_until IS NULL`),
		"tentativa contada, causa registrada, próxima agendada e lease devolvido")
}

// AC-4
func TestEventoComLeaseVivoDeOutraInstanciaEIgnorado(t *testing.T) {
	a := novoAmbienteOutbox(t)
	a.carteira(t, "100.00")
	a.executar(t, `UPDATE outbox_events
	                  SET locked_by = 'outra-instancia', locked_until = now() + interval '5 minutes'`)

	publicados, err := a.dispatcher.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Zero(t, publicados, "trabalho de outra instância não se toma no meio")
	assert.Empty(t, a.publicador.recebidas())
}

// AC-5 — recuperação de trabalho abandonado.
func TestEventoComLeaseExpiradoEAssumido(t *testing.T) {
	a := novoAmbienteOutbox(t)
	a.carteira(t, "100.00")
	a.executar(t, `UPDATE outbox_events
	                  SET locked_by = 'instancia-que-morreu', locked_until = now() - interval '1 minute'`)

	publicados, err := a.dispatcher.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Equal(t, 2, publicados,
		"uma instância que morreu segurando eventos não pode travá-los para sempre")
	assert.Zero(t, a.conta(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`))
}

// AC-7 — queda entre publicar e confirmar.
func TestQuedaEntrePublicarEConfirmarRepublicaComOMesmoEventID(t *testing.T) {
	a := novoAmbienteOutbox(t)
	a.carteira(t, "100.00")

	quebrado := &repoQueFalhaAoConfirmar{Repository: a.repo, falhar: true}
	caido := outbox.NewDispatcher(quebrado, a.publicador,
		slog.New(slog.NewTextHandler(io.Discard, nil)), "instancia-que-caiu", 50, time.Minute)

	publicados, err := caido.RunOnce(a.ctx)
	require.NoError(t, err)
	assert.Zero(t, publicados, "sem confirmação, a rodada não conta como publicada")
	primeira := a.publicador.recebidas()
	require.Len(t, primeira, 2, "as mensagens SAÍRAM: a queda foi depois do envio")

	// A instância morreu segurando o lease; ele expira e outra assume.
	a.executar(t, `UPDATE outbox_events SET locked_until = now() - interval '1 minute'`)

	republicados, err := a.dispatcher.RunOnce(a.ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, republicados)

	segunda := a.publicador.recebidas()
	require.Len(t, segunda, 4, "at-least-once: o evento sai de novo")
	assert.Equal(t, primeira[0].DeduplicationID, segunda[2].DeduplicationID,
		"o eventId é o MESMO — é isso que faz a repetição ser inócua para o consumidor")
	assert.Equal(t, primeira[1].DeduplicationID, segunda[3].DeduplicationID)
}

// AC-9
func TestEnvelopePublicadoTemOsCamposDoContrato(t *testing.T) {
	a := novoAmbienteOutbox(t)
	w, _ := a.carteira(t, "100.00")

	_, err := a.dispatcher.RunOnce(a.ctx)
	require.NoError(t, err)

	msgs := a.publicador.recebidas()
	require.NotEmpty(t, msgs)

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(msgs[0].Body, &envelope))

	for _, campo := range []string{
		"eventId", "eventType", "aggregateType", "aggregateId",
		"correlationId", "causationId", "occurredAt", "version", "data",
	} {
		assert.Contains(t, envelope, campo)
	}
	assert.Equal(t, "wallet", envelope["aggregateType"])
	assert.Equal(t, w.String(), envelope["aggregateId"])
	assert.Equal(t, msgs[0].DeduplicationID, envelope["eventId"],
		"o identificador de deduplicação tem de ser o mesmo eventId do corpo")
	assert.Equal(t, msgs[0].GroupID, envelope["aggregateId"])

	ocorrido, ok := envelope["occurredAt"].(string)
	require.True(t, ok)
	_, err = time.Parse("2006-01-02T15:04:05.000Z07:00", ocorrido)
	assert.NoError(t, err, "occurredAt tem de estar em RFC 3339 UTC com precisão fixa")
}

// AC-E1
func TestOutboxVaziaTerminaSemErro(t *testing.T) {
	a := novoAmbienteOutbox(t)

	publicados, err := a.dispatcher.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Zero(t, publicados)
}

// AC-E2 — um evento envenenado não pode segurar os outros, nem ser abandonado.
func TestEventoQueSempreFalhaNaoBloqueiaOsDemais(t *testing.T) {
	a := novoAmbienteOutbox(t)
	a.carteira(t, "100.00")

	var envenenado string
	require.NoError(t, a.db.Session(a.ctx).
		Raw(`SELECT event_id FROM outbox_events ORDER BY created_at LIMIT 1`).
		Scan(&envenenado).Error)
	a.publicador.falharPor = map[string]error{envenenado: errors.New("payload recusado")}

	publicados, err := a.dispatcher.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, publicados, "o evento saudável sai mesmo com o vizinho falhando")
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE event_id = ?::uuid AND published_at IS NULL AND attempts = 1`, envenenado),
		"o envenenado continua pendente, contado e visível — nunca descartado")

	// E a rodada seguinte não fica presa nele: ele está agendado para o futuro.
	segunda, err := a.dispatcher.RunOnce(a.ctx)
	require.NoError(t, err)
	assert.Zero(t, segunda)
}
