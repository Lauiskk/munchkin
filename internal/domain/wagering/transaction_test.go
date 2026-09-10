package wagering_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

var agora = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, v string) money.Money {
	t.Helper()
	m, err := money.Parse(v, money.BRL)
	require.NoError(t, err)
	return m
}

func partes(t *testing.T) (wagering.TransactionID, wallet.ID, wallet.PlayerID) {
	t.Helper()
	tid, err := wagering.NewTransactionID()
	require.NoError(t, err)
	wid, err := wallet.NewID()
	require.NoError(t, err)
	pid, err := wallet.NewPlayerID()
	require.NoError(t, err)
	return tid, wid, pid
}

func origem() wagering.ExternalOrigin {
	return wagering.ExternalOrigin{
		ProviderID:     "provider-a",
		ExternalID:     "tx-1",
		IdempotencyKey: "provider-a:tx-1",
		PayloadHash:    []byte{0x01, 0x02},
		RoundID:        "round-987",
		GameID:         "fortune-chimp",
	}
}

func externa(t *testing.T, kind wagering.Kind, valor string, ref wagering.ExternalID) *wagering.Transaction {
	t.Helper()
	tid, wid, pid := partes(t)
	tx, err := wagering.NewExternal(tid, kind, wid, pid, brl(t, valor), origem(), ref, agora)
	require.NoError(t, err)
	return tx
}

func TestOperacaoExternaNasceEmPendente(t *testing.T) {
	tx := externa(t, wagering.Bet, "25.00", "")

	assert.Equal(t, wagering.Pending, tx.Status())
	assert.Equal(t, wagering.Bet, tx.Kind())
	assert.True(t, tx.IsExternal())
	require.NotNil(t, tx.Origin())
	assert.Equal(t, wagering.ProviderID("provider-a"), tx.Origin().ProviderID)
}

// AC-E5: um provedor que pudesse enviar OPENING creditaria a própria carteira
// sem passar pelo caminho de abertura.
func TestAberturaInternaNaoAceitaOrigemExterna(t *testing.T) {
	tid, wid, pid := partes(t)

	_, err := wagering.NewExternal(tid, wagering.Opening, wid, pid,
		brl(t, "100.00"), origem(), "", agora)

	require.Error(t, err)
	assert.ErrorIs(t, err, wagering.ErrInvalidKind)
}

func TestAberturaInternaNaoCarregaMetadadosExternos(t *testing.T) {
	tid, wid, pid := partes(t)

	tx, err := wagering.NewOpening(tid, wid, pid, brl(t, "1000.00"), agora)
	require.NoError(t, err)

	assert.False(t, tx.IsExternal())
	assert.Nil(t, tx.Origin())
	assert.Empty(t, tx.ReferenceExternalID())
}

func TestOperacaoExternaExigeMetadadosCompletos(t *testing.T) {
	tid, wid, pid := partes(t)

	incompletas := map[string]func(o *wagering.ExternalOrigin){
		"sem provedor":   func(o *wagering.ExternalOrigin) { o.ProviderID = "" },
		"sem id externo": func(o *wagering.ExternalOrigin) { o.ExternalID = "" },
		"sem chave":      func(o *wagering.ExternalOrigin) { o.IdempotencyKey = "" },
		"sem hash":       func(o *wagering.ExternalOrigin) { o.PayloadHash = nil },
		"sem rodada":     func(o *wagering.ExternalOrigin) { o.RoundID = "" },
		"sem jogo":       func(o *wagering.ExternalOrigin) { o.GameID = "" },
	}
	for nome, remover := range incompletas {
		t.Run(nome, func(t *testing.T) {
			o := origem()
			remover(&o)
			_, err := wagering.NewExternal(tid, wagering.Bet, wid, pid, brl(t, "25.00"), o, "", agora)
			assert.ErrorIs(t, err, wagering.ErrMissingOrigin)
		})
	}
}

// AC-10
func TestValorExigidoPorTipo(t *testing.T) {
	tid, wid, pid := partes(t)

	t.Run("LOSS exige valor zero", func(t *testing.T) {
		_, err := wagering.NewExternal(tid, wagering.Loss, wid, pid,
			brl(t, "5.00"), origem(), "", agora)
		assert.ErrorIs(t, err, wagering.ErrInvalidAmountForKind)

		_, err = wagering.NewExternal(tid, wagering.Loss, wid, pid,
			brl(t, "0.00"), origem(), "", agora)
		assert.NoError(t, err)
	})

	for _, kind := range []wagering.Kind{wagering.Bet, wagering.Win} {
		t.Run(kind.String()+" exige valor positivo", func(t *testing.T) {
			_, err := wagering.NewExternal(tid, kind, wid, pid,
				brl(t, "0.00"), origem(), "", agora)
			assert.ErrorIs(t, err, wagering.ErrInvalidAmountForKind)
		})
	}
}

// Três regras, não duas: reversão EXIGE referência, WIN PODE ter, BET e LOSS
// não podem. A terceira direção é a que protege o resolvedor — sem ela uma
// aposta com referência passaria e alguém tentaria resolvê-la.
func TestQuemPodeCarregarReferencia(t *testing.T) {
	tid, wid, pid := partes(t)

	for _, kind := range []wagering.Kind{wagering.Refund, wagering.Rollback} {
		t.Run(kind.String()+" sem referência", func(t *testing.T) {
			_, err := wagering.NewExternal(tid, kind, wid, pid,
				brl(t, "25.00"), origem(), "", agora)
			assert.ErrorIs(t, err, wagering.ErrMissingReference)
		})
	}

	for _, kind := range []wagering.Kind{wagering.Bet, wagering.Loss} {
		t.Run(kind.String()+" com referência", func(t *testing.T) {
			valor := "25.00"
			if kind == wagering.Loss {
				valor = "0.00"
			}
			_, err := wagering.NewExternal(tid, kind, wid, pid,
				brl(t, valor), origem(), "tx-original", agora)
			assert.ErrorIs(t, err, wagering.ErrUnexpectedReference)
		})
	}

	// WIN é a exceção que o §7 do enunciado abre: ele PODE informar a aposta da
	// mesma rodada. A referência fica gravada e não é resolvida — este teste
	// afirmava o contrário antes, e uma auditoria contra o enunciado mostrou que
	// era o teste, e não o enunciado, que estava errado.
	t.Run("WIN aceita referência e a preserva", func(t *testing.T) {
		tx, err := wagering.NewExternal(tid, wagering.Win, wid, pid,
			brl(t, "25.00"), origem(), "aposta-original", agora)

		require.NoError(t, err)
		assert.Equal(t, wagering.ExternalID("aposta-original"), tx.ReferenceExternalID())
		assert.Equal(t, wagering.Pending, tx.Status(),
			"a referência não muda o ponto de partida")
	})

	// E o ganho continua sendo crédito imediato: quem espera e quem resolve são
	// os dois guardas presos a reversão, e é o que impede a referência
	// informativa de virar pendência.
	t.Run("WIN com referência não espera nem resolve", func(t *testing.T) {
		tx, err := wagering.NewExternal(tid, wagering.Win, wid, pid,
			brl(t, "25.00"), origem(), "aposta-original", agora)
		require.NoError(t, err)

		assert.ErrorIs(t, tx.MarkPendingReference(agora, agora),
			wagering.ErrUnexpectedReference)

		outra, err := wagering.NewTransactionID()
		require.NoError(t, err)
		assert.ErrorIs(t, tx.ResolveReference(outra), wagering.ErrUnexpectedReference)
	})
}

func TestMaquinaDeEstados(t *testing.T) {
	t.Run("conclusão guarda o saldo observado", func(t *testing.T) {
		tx := externa(t, wagering.Bet, "25.00", "")

		require.NoError(t, tx.MarkProcessed(brl(t, "975.00"), agora))

		assert.Equal(t, wagering.Processed, tx.Status())
		saldo, ok := tx.ResultBalance()
		require.True(t, ok, "o replay devolve o saldo do processamento original")
		assert.Equal(t, "975.00", saldo.Amount())
		_, temInstante := tx.ProcessedAt()
		assert.True(t, temInstante)
	})

	t.Run("recusa guarda o código de falha", func(t *testing.T) {
		tx := externa(t, wagering.Bet, "25.00", "")

		require.NoError(t, tx.MarkRejected(wagering.FailureInsufficientFunds, agora))

		assert.Equal(t, wagering.Rejected, tx.Status())
		assert.Equal(t, wagering.FailureInsufficientFunds, tx.FailureCode())
	})

	t.Run("código de falha desconhecido é recusado", func(t *testing.T) {
		tx := externa(t, wagering.Bet, "25.00", "")
		err := tx.MarkRejected(wagering.FailureCode("INVENTADO"), agora)
		require.Error(t, err)
		assert.Equal(t, wagering.Pending, tx.Status(), "a recusa não pode ter transicionado")
	})

	// AC-9 e AC-E4
	t.Run("estado terminal não sofre nova transição", func(t *testing.T) {
		terminais := map[string]func(*wagering.Transaction) error{
			"processada": func(tx *wagering.Transaction) error {
				return tx.MarkProcessed(brl(t, "10.00"), agora)
			},
			"recusada": func(tx *wagering.Transaction) error {
				return tx.MarkRejected(wagering.FailureInsufficientFunds, agora)
			},
			"falha permanente": func(tx *wagering.Transaction) error {
				return tx.MarkFailed(wagering.FailureInternalError, agora)
			},
		}
		for nome, terminar := range terminais {
			t.Run(nome, func(t *testing.T) {
				tx := externa(t, wagering.Bet, "25.00", "")
				require.NoError(t, terminar(tx))
				estadoFinal := tx.Status()

				// Nenhuma transição a partir daqui, nem para o mesmo estado.
				assert.ErrorIs(t, tx.MarkProcessed(brl(t, "1.00"), agora), wagering.ErrTerminal)
				assert.ErrorIs(t, tx.MarkRejected(wagering.FailureInsufficientFunds, agora), wagering.ErrTerminal)
				assert.ErrorIs(t, tx.MarkFailed(wagering.FailureInternalError, agora), wagering.ErrTerminal)
				assert.Equal(t, estadoFinal, tx.Status(), "o estado não pode ter mudado")
			})
		}
	})

	t.Run("espera por referência e retomada", func(t *testing.T) {
		tx := externa(t, wagering.Refund, "25.00", "tx-original")
		depois := agora.Add(time.Minute)

		require.NoError(t, tx.MarkPendingReference(depois, agora))
		assert.Equal(t, wagering.PendingReference, tx.Status())
		quando, ok := tx.NextAttemptAt()
		require.True(t, ok)
		assert.Equal(t, depois.UTC(), quando)
		assert.Zero(t, tx.Attempts())

		require.NoError(t, tx.ScheduleRetry(agora.Add(2*time.Minute), agora))
		assert.Equal(t, 1, tx.Attempts())

		// De PENDING_REFERENCE ainda se pode concluir ou recusar.
		require.NoError(t, tx.MarkProcessed(brl(t, "100.00"), agora))
		assert.Equal(t, wagering.Processed, tx.Status())
	})

	// A máquina recusa transição não declarada mesmo entre estados NÃO
	// terminais — que é o caso que a verificação de terminalidade não alcança.
	// Sem este teste, remover a consulta à máquina passaria despercebido.
	t.Run("transição não declarada entre estados não terminais", func(t *testing.T) {
		tx := externa(t, wagering.Refund, "25.00", "tx-original")
		require.NoError(t, tx.MarkPendingReference(agora.Add(time.Minute), agora))
		require.Equal(t, wagering.PendingReference, tx.Status())
		require.False(t, tx.Status().IsTerminal(), "o estado precisa ser não terminal")

		// Reentrar no mesmo estado não é transição válida: a retentativa é
		// ScheduleRetry, que não passa pela máquina.
		err := tx.MarkPendingReference(agora.Add(2*time.Minute), agora)
		assert.ErrorIs(t, err, wagering.ErrInvalidTransition)
		assert.Equal(t, wagering.PendingReference, tx.Status())
	})

	t.Run("a máquina declara exatamente as transições permitidas", func(t *testing.T) {
		permitidas := map[wagering.Status][]wagering.Status{
			wagering.Pending: {
				wagering.PendingReference, wagering.Processed,
				wagering.Rejected, wagering.Failed,
			},
			wagering.PendingReference: {
				wagering.Processed, wagering.Rejected, wagering.Failed,
			},
		}
		todos := []wagering.Status{
			wagering.Pending, wagering.PendingReference,
			wagering.Processed, wagering.Rejected, wagering.Failed,
		}

		for origem, destinos := range permitidas {
			permitido := map[wagering.Status]bool{}
			for _, d := range destinos {
				permitido[d] = true
				assert.True(t, origem.CanTransitionTo(d), "%s -> %s deveria ser permitida", origem, d)
			}
			for _, d := range todos {
				if !permitido[d] {
					assert.False(t, origem.CanTransitionTo(d),
						"%s -> %s NÃO deveria ser permitida", origem, d)
				}
			}
		}

		for _, terminal := range []wagering.Status{wagering.Processed, wagering.Rejected, wagering.Failed} {
			assert.True(t, terminal.IsTerminal(), "%s é terminal", terminal)
			for _, d := range todos {
				assert.False(t, terminal.CanTransitionTo(d),
					"estado terminal %s não transiciona para %s", terminal, d)
			}
		}
	})

	t.Run("só reversão espera por referência", func(t *testing.T) {
		tx := externa(t, wagering.Bet, "25.00", "")
		err := tx.MarkPendingReference(agora, agora)
		assert.ErrorIs(t, err, wagering.ErrUnexpectedReference)
	})

	t.Run("retentativa exige estar aguardando referência", func(t *testing.T) {
		tx := externa(t, wagering.Refund, "25.00", "tx-original")
		err := tx.ScheduleRetry(agora, agora)
		assert.ErrorIs(t, err, wagering.ErrInvalidTransition)
	})
}

func TestResolucaoDeReferencia(t *testing.T) {
	tx := externa(t, wagering.Refund, "25.00", "tx-original")
	ref, err := wagering.NewTransactionID()
	require.NoError(t, err)

	_, resolvida := tx.ReferenceID()
	assert.False(t, resolvida)

	require.NoError(t, tx.ResolveReference(ref))
	obtida, resolvida := tx.ReferenceID()
	require.True(t, resolvida)
	assert.Equal(t, ref, obtida)

	t.Run("operação sem referência não resolve nada", func(t *testing.T) {
		aposta := externa(t, wagering.Bet, "25.00", "")
		assert.ErrorIs(t, aposta.ResolveReference(ref), wagering.ErrUnexpectedReference)
	})
}

// AC-E1
func TestTransacaoNaoInicializadaEhRecusada(t *testing.T) {
	var vazia wagering.Transaction

	assert.ErrorIs(t, vazia.MarkProcessed(brl(t, "10.00"), agora), wagering.ErrUninitialized)
	assert.ErrorIs(t, vazia.ResolveReference(wagering.TransactionID{}), wagering.ErrUninitialized)
}

func TestReidratacaoPreservaOEstadoPersistido(t *testing.T) {
	tid, wid, pid := partes(t)
	saldo := brl(t, "975.00")
	o := origem()

	tx, err := wagering.Rehydrate(wagering.State{
		ID: tid, Kind: wagering.Bet, Status: wagering.Processed,
		WalletID: wid, PlayerID: pid, Amount: brl(t, "25.00"),
		Origin: &o, ResultBalance: &saldo,
		CreatedAt: agora, UpdatedAt: agora,
	})
	require.NoError(t, err)

	assert.Equal(t, wagering.Processed, tx.Status())
	obtido, ok := tx.ResultBalance()
	require.True(t, ok)
	assert.Equal(t, "975.00", obtido.Amount())

	// E o estado reidratado continua respeitando a máquina: terminal é terminal.
	assert.ErrorIs(t, tx.MarkRejected(wagering.FailureInsufficientFunds, agora), wagering.ErrTerminal)
}

func TestOrigemDevolvidaEhCopia(t *testing.T) {
	tx := externa(t, wagering.Bet, "25.00", "")

	o := tx.Origin()
	require.NotNil(t, o)
	o.ProviderID = "provider-invasor"

	assert.Equal(t, wagering.ProviderID("provider-a"), tx.Origin().ProviderID,
		"alterar a cópia não pode alterar o agregado")
}

// A reidratação valida o que vem do banco. O CHECK do schema cobre tipo e
// estado; o código de falha não tem CHECK, e um valor desconhecido devolvido ao
// provedor quebraria a integração dele sem erro nenhum do nosso lado.
func TestReidratacaoRecusaEstadoPersistidoInvalido(t *testing.T) {
	tid, wid, pid := partes(t)
	base := wagering.State{
		ID: tid, Kind: wagering.Bet, Status: wagering.Pending,
		WalletID: wid, PlayerID: pid, Amount: brl(t, "25.00"),
		CreatedAt: agora, UpdatedAt: agora,
	}

	t.Run("tipo desconhecido", func(t *testing.T) {
		s := base
		s.Kind = "TRANSFER"
		_, err := wagering.Rehydrate(s)
		assert.ErrorIs(t, err, wagering.ErrInvalidKind)
	})

	t.Run("estado desconhecido", func(t *testing.T) {
		s := base
		s.Status = "EM_ANALISE"
		_, err := wagering.Rehydrate(s)
		assert.ErrorIs(t, err, wagering.ErrInvalidStatus)
	})

	t.Run("código de falha desconhecido", func(t *testing.T) {
		s := base
		s.Status = wagering.Rejected
		s.FailureCode = "MOTIVO_INVENTADO"
		_, err := wagering.Rehydrate(s)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "código de falha desconhecido")
	})

	t.Run("código de falha conhecido é aceito", func(t *testing.T) {
		s := base
		s.Status = wagering.Rejected
		s.FailureCode = wagering.FailureInsufficientFunds
		_, err := wagering.Rehydrate(s)
		assert.NoError(t, err)
	})
}

// A tabela de reversões é contrato: acrescentar uma combinação permite desfazer
// algo que hoje não se desfaz, e isso não pode acontecer sem intenção.
func TestTabelaDeReversoes(t *testing.T) {
	permitido := map[wagering.Kind][]wagering.Kind{
		wagering.Refund:   {wagering.Bet},
		wagering.Rollback: {wagering.Bet, wagering.Win, wagering.Refund},
	}
	todos := []wagering.Kind{
		wagering.Opening, wagering.Bet, wagering.Win,
		wagering.Loss, wagering.Refund, wagering.Rollback,
	}

	for reversao, alvos := range permitido {
		aceito := map[wagering.Kind]bool{}
		for _, a := range alvos {
			aceito[a] = true
			assert.True(t, reversao.CanReverse(a), "%s deveria reverter %s", reversao, a)
		}
		for _, a := range todos {
			if !aceito[a] {
				assert.False(t, reversao.CanReverse(a),
					"%s NÃO deveria reverter %s", reversao, a)
			}
		}
	}

	// O que não é reversão não reverte nada.
	for _, k := range []wagering.Kind{wagering.Opening, wagering.Bet, wagering.Win, wagering.Loss} {
		for _, alvo := range todos {
			assert.False(t, k.CanReverse(alvo), "%s não é reversão", k)
		}
	}
}

// A direção da reversão é o movimento contrário ao original.
func TestDirecaoDaMovimentacaoPorTipo(t *testing.T) {
	assert.False(t, wagering.Bet.CreditsWallet(), "aposta debita")
	assert.True(t, wagering.Win.CreditsWallet(), "ganho credita")
	assert.True(t, wagering.Refund.CreditsWallet(), "estorno credita")
	assert.False(t, wagering.Loss.CreditsWallet())
}
