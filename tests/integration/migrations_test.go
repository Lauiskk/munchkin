//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/config"
)

func novoMigrator(t *testing.T, dbCfg config.DB) *postgres.Migrator {
	t.Helper()
	m, err := postgres.NewMigrator(asOwner(dbCfg), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// migrado sobe um PostgreSQL e aplica todas as migrations.
func migrado(t *testing.T) config.DB {
	t.Helper()
	dbCfg := startPostgres(t)
	require.NoError(t, novoMigrator(t, dbCfg).Up())
	return dbCfg
}

func TestCicloDeMigracao(t *testing.T) {
	dbCfg := startPostgres(t)
	m := novoMigrator(t, dbCfg)

	versao, sujo, err := m.Version()
	require.NoError(t, err)
	assert.Zero(t, versao, "banco novo começa sem versão")
	assert.False(t, sujo)

	require.NoError(t, m.Up())
	versao, sujo, err = m.Version()
	require.NoError(t, err)
	assert.Equal(t, uint(6), versao)
	assert.False(t, sujo)

	// Aplicar de novo não pode falhar: o executor roda em toda subida do
	// ambiente, e um erro aqui viraria falha de deploy sem nada errado.
	require.NoError(t, m.Up(), "aplicar migrations já aplicadas é operação sem efeito")

	require.NoError(t, m.Down(), "reverter uma etapa")
	versao, _, err = m.Version()
	require.NoError(t, err)
	assert.Equal(t, uint(5), versao)

	require.NoError(t, m.Up())

	// A reversão completa precisa limpar tudo, inclusive as funções dos
	// gatilhos — função órfã faz a próxima aplicação falhar ou, pior, silenciar.
	require.NoError(t, m.DownAll())

	db := abrir(t, asOwner(dbCfg))
	ctx := context.Background()

	var tabelas int64
	require.NoError(t, db.Session(ctx).Raw(
		`SELECT count(*) FROM pg_tables
		  WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`).
		Scan(&tabelas).Error)
	assert.Zero(t, tabelas, "nenhuma tabela do domínio pode sobrar")

	var funcoes int64
	require.NoError(t, db.Session(ctx).Raw(
		`SELECT count(*) FROM pg_proc p
		   JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE n.nspname = 'public'`).
		Scan(&funcoes).Error)
	assert.Zero(t, funcoes, "nenhuma função de gatilho pode sobrar")

	require.NoError(t, m.Up(), "o ciclo tem que ser repetível")
}

// As invariantes vivem no schema. Cada caso abaixo é uma violação que a
// aplicação nunca deveria tentar — e que, se tentasse, o banco recusa.
func TestSchemaRecusaViolacoes(t *testing.T) {
	dbCfg := migrado(t)
	app := abrir(t, dbCfg)
	ctx := context.Background()

	// criarCarteira devolve os identificadores de uma carteira nova.
	criarCarteira := func(t *testing.T, saldo int64) (walletID, playerID string) {
		t.Helper()
		walletID, playerID = uuid.NewString(), uuid.NewString()
		require.NoError(t, app.Session(ctx).Exec(
			`INSERT INTO wallets (id, player_id, currency, balance_minor, version)
			 VALUES (?, ?, 'BRL', ?, 1)`, walletID, playerID, saldo).Error)
		return walletID, playerID
	}

	t.Run("saldo não pode ficar negativo", func(t *testing.T) {
		w, _ := criarCarteira(t, 10000)
		err := app.Session(ctx).Exec(`UPDATE wallets SET balance_minor = -1 WHERE id = ?`, w).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wallets_balance_non_negative")
	})

	t.Run("uma carteira por jogador e moeda", func(t *testing.T) {
		_, p := criarCarteira(t, 0)
		err := app.Session(ctx).Exec(
			`INSERT INTO wallets (id, player_id, currency, balance_minor, version)
			 VALUES (?, ?, 'BRL', 0, 1)`, uuid.NewString(), p).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wallets_player_currency_uk")
	})

	t.Run("moeda fora do ISO 4217", func(t *testing.T) {
		err := app.Session(ctx).Exec(
			`INSERT INTO wallets (id, player_id, currency, balance_minor, version)
			 VALUES (?, ?, 'br', 0, 1)`, uuid.NewString(), uuid.NewString()).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wallets_currency_iso4217")
	})

	// Sem esta restrição, um provedor enviaria OPENING por HTTP e creditaria a
	// própria carteira. É a fronteira entre origem interna e externa.
	t.Run("abertura interna não aceita metadados de provedor", func(t *testing.T) {
		w, p := criarCarteira(t, 0)
		err := app.Session(ctx).Exec(`
			INSERT INTO wager_transactions
			  (id, kind, status, wallet_id, player_id, amount_minor, currency,
			   provider_id, external_transaction_id, idempotency_key, payload_hash,
			   round_id, game_id, result_balance_minor, processed_at)
			VALUES (?, 'OPENING', 'PROCESSED', ?, ?, 100, 'BRL',
			        'provider-a', 'x', 'k', '\x00', 'r', 'g', 100, now())`,
			uuid.NewString(), w, p).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_origin")
	})

	t.Run("operação externa exige metadados completos", func(t *testing.T) {
		w, p := criarCarteira(t, 0)
		err := app.Session(ctx).Exec(`
			INSERT INTO wager_transactions
			  (id, kind, status, wallet_id, player_id, amount_minor, currency)
			VALUES (?, 'BET', 'PENDING', ?, ?, 100, 'BRL')`,
			uuid.NewString(), w, p).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_origin")
	})

	t.Run("LOSS exige valor zero e as demais valor positivo", func(t *testing.T) {
		w, p := criarCarteira(t, 0)
		err := inserirExterna(app.Session(ctx), w, p, "LOSS", "PENDING", 500, "BRL", "l-nz")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_amount_by_kind")

		require.NoError(t, inserirExterna(app.Session(ctx), w, p, "LOSS", "PENDING", 0, "BRL", "l-ok"))

		err = inserirExterna(app.Session(ctx), w, p, "BET", "PENDING", 0, "BRL", "b-zero")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_amount_by_kind")
	})

	// A chave estrangeira composta torna a divergência de moeda impossível, em
	// vez de deixá-la como algo que o domínio precisa lembrar de conferir.
	t.Run("movimentação em moeda diferente da carteira", func(t *testing.T) {
		w, p := criarCarteira(t, 0)
		err := inserirExterna(app.Session(ctx), w, p, "BET", "PENDING", 500, "USD", "b-usd")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_wallet_currency_fk")
	})

	t.Run("estado de recusa exige código de falha", func(t *testing.T) {
		w, p := criarCarteira(t, 0)
		err := inserirExterna(app.Session(ctx), w, p, "BET", "REJECTED", 500, "BRL", "b-rej")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_failure_code")
	})
}

func TestIdempotenciaEhImpostaPeloIndice(t *testing.T) {
	dbCfg := migrado(t)
	app := abrir(t, dbCfg)
	ctx := context.Background()

	w, p := uuid.NewString(), uuid.NewString()
	require.NoError(t, app.Session(ctx).Exec(
		`INSERT INTO wallets (id, player_id, currency, balance_minor, version)
		 VALUES (?, ?, 'BRL', 10000, 1)`, w, p).Error)

	require.NoError(t, inserirExterna(app.Session(ctx), w, p, "BET", "PENDING", 8000, "BRL", "tx-1"))

	t.Run("mesma operação com outra chave é recusada", func(t *testing.T) {
		err := app.Session(ctx).Exec(`
			INSERT INTO wager_transactions
			  (id, kind, status, wallet_id, player_id, amount_minor, currency,
			   provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id)
			VALUES (?, 'BET', 'PENDING', ?, ?, 8000, 'BRL',
			        'provider-a', 'tx-1', 'chave-diferente', '\x02', 'r', 'g')`,
			uuid.NewString(), w, p).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_provider_external_uk")
	})

	t.Run("mesma chave para outra operação é recusada", func(t *testing.T) {
		err := app.Session(ctx).Exec(`
			INSERT INTO wager_transactions
			  (id, kind, status, wallet_id, player_id, amount_minor, currency,
			   provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id)
			VALUES (?, 'BET', 'PENDING', ?, ?, 8000, 'BRL',
			        'provider-a', 'tx-outra', 'provider-a:tx-1', '\x03', 'r', 'g')`,
			uuid.NewString(), w, p).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_provider_key_uk")
	})

	// O escopo da unicidade é por provedor: dois provedores podem usar o mesmo
	// identificador externo sem colidir, porque são operações distintas.
	t.Run("o mesmo identificador em outro provedor é aceito", func(t *testing.T) {
		err := app.Session(ctx).Exec(`
			INSERT INTO wager_transactions
			  (id, kind, status, wallet_id, player_id, amount_minor, currency,
			   provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id)
			VALUES (?, 'BET', 'PENDING', ?, ?, 100, 'BRL',
			        'provider-b', 'tx-1', 'provider-b:tx-1', '\x04', 'r', 'g')`,
			uuid.NewString(), w, p).Error
		require.NoError(t, err)
	})

	t.Run("uma só abertura por carteira", func(t *testing.T) {
		abrir := func(id string) error {
			return app.Session(ctx).Exec(`
				INSERT INTO wager_transactions
				  (id, kind, status, wallet_id, player_id, amount_minor, currency,
				   result_balance_minor, processed_at)
				VALUES (?, 'OPENING', 'PROCESSED', ?, ?, 10000, 'BRL', 10000, now())`,
				id, w, p).Error
		}
		require.NoError(t, abrir(uuid.NewString()))

		err := abrir(uuid.NewString())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wager_transactions_opening_uk",
			"crédito inicial duplicado precisa ser impossível")
	})
}

// Interpretação adotada: no máximo UMA reversão bem-sucedida por referência,
// de qualquer tipo. A leitura literal do enunciado permitiria um REFUND e um
// ROLLBACK sobre a mesma aposta, o que devolveria o dinheiro duas vezes.
func TestUmaSoReversaoBemSucedidaPorReferencia(t *testing.T) {
	dbCfg := migrado(t)
	app := abrir(t, dbCfg)
	ctx := context.Background()

	w, p := uuid.NewString(), uuid.NewString()
	aposta := uuid.NewString()
	require.NoError(t, app.Session(ctx).Exec(
		`INSERT INTO wallets (id, player_id, currency, balance_minor, version)
		 VALUES (?, ?, 'BRL', 10000, 1)`, w, p).Error)
	require.NoError(t, app.Session(ctx).Exec(`
		INSERT INTO wager_transactions
		  (id, kind, status, wallet_id, player_id, amount_minor, currency,
		   provider_id, external_transaction_id, idempotency_key, payload_hash,
		   round_id, game_id, result_balance_minor, processed_at)
		VALUES (?, 'BET', 'PROCESSED', ?, ?, 8000, 'BRL',
		        'provider-a', 'tx-1', 'provider-a:tx-1', '\x01', 'r', 'g', 2000, now())`,
		aposta, w, p).Error)

	reverter := func(kind, externalID string) error {
		return app.Session(ctx).Exec(`
			INSERT INTO wager_transactions
			  (id, kind, status, wallet_id, player_id, amount_minor, currency,
			   provider_id, external_transaction_id, idempotency_key, payload_hash,
			   round_id, game_id, reference_external_transaction_id, reference_transaction_id,
			   result_balance_minor, processed_at)
			VALUES (?, ?, 'PROCESSED', ?, ?, 8000, 'BRL',
			        'provider-a', ?, ?, '\x02', 'r', 'g', 'tx-1', ?, 10000, now())`,
			uuid.NewString(), kind, w, p, externalID, "k-"+externalID, aposta).Error
	}

	require.NoError(t, reverter("REFUND", "rf-1"), "a primeira reversão passa")

	err := reverter("REFUND", "rf-2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wager_transactions_single_reversal_uk")

	err = reverter("ROLLBACK", "rb-1")
	require.Error(t, err, "ROLLBACK de aposta já estornada devolveria o mesmo débito duas vezes")
	assert.Contains(t, err.Error(), "wager_transactions_single_reversal_uk")
}

// inserirExterna insere uma transação de origem externa com metadados mínimos.
func inserirExterna(tx *gorm.DB, wallet, player, kind, status string, valor int64, moeda, externalID string) error {
	return tx.Exec(`
		INSERT INTO wager_transactions
		  (id, kind, status, wallet_id, player_id, amount_minor, currency,
		   provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'provider-a', ?, ?, '\x01', 'r', 'g')`,
		uuid.NewString(), kind, status, wallet, player, valor, moeda,
		externalID, "provider-a:"+externalID).Error
}

// O ledger é append-only imposto pelo banco, em duas camadas independentes:
// privilégio revogado do papel da aplicação, e gatilho que recusa a operação
// mesmo para o dono do schema. Uma camada só não bastaria — a de privilégio não
// alcança o dono, e a de gatilho poderia ser removida por quem tem DDL.
func TestLedgerEhAppendOnlyNasDuasCamadas(t *testing.T) {
	dbCfg := migrado(t)
	app := abrir(t, dbCfg)
	dono := abrir(t, asOwner(dbCfg))
	ctx := context.Background()

	w, p := uuid.NewString(), uuid.NewString()
	tx := uuid.NewString()
	require.NoError(t, app.Session(ctx).Exec(
		`INSERT INTO wallets (id, player_id, currency, balance_minor, version)
		 VALUES (?, ?, 'BRL', 10000, 1)`, w, p).Error)
	require.NoError(t, inserirExterna(app.Session(ctx), w, p, "BET", "PENDING", 8000, "BRL", "tx-1"))
	require.NoError(t, app.Session(ctx).Exec(
		`UPDATE wager_transactions SET id = ? WHERE external_transaction_id = 'tx-1'`, tx).Error)

	lancar := func(direcao string, valor, antes, depois int64) error {
		return app.Session(ctx).Exec(`
			INSERT INTO wallet_ledger_entries
			  (id, wallet_id, transaction_id, direction, amount_minor, currency,
			   balance_before_minor, balance_after_minor)
			VALUES (?, ?, ?, ?, ?, 'BRL', ?, ?)`,
			uuid.NewString(), w, tx, direcao, valor, antes, depois).Error
	}

	t.Run("a aritmética do lançamento é conferida pelo banco", func(t *testing.T) {
		err := lancar("DEBIT", 8000, 10000, 9999)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wallet_ledger_entries_arithmetic",
			"lançamento que não fecha a conta não pode entrar: a reconciliação depende disso")
	})

	require.NoError(t, lancar("DEBIT", 8000, 10000, 2000))

	t.Run("um lançamento por transação e carteira", func(t *testing.T) {
		err := lancar("DEBIT", 8000, 10000, 2000)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wallet_ledger_entries_wallet_transaction_uk",
			"é o que impede movimentação duplicada se a mesma transação for reaplicada")
	})

	t.Run("a aplicação não tem privilégio de alterar nem apagar", func(t *testing.T) {
		err := app.Session(ctx).Exec(`UPDATE wallet_ledger_entries SET amount_minor = 1`).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")

		err = app.Session(ctx).Exec(`DELETE FROM wallet_ledger_entries`).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")
	})

	// Esta é a camada que o privilégio não alcança: contra o dono, quem
	// responde é o gatilho.
	t.Run("nem o dono do schema consegue alterar", func(t *testing.T) {
		err := dono.Session(ctx).Exec(`UPDATE wallet_ledger_entries SET amount_minor = 1`).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "append-only")

		err = dono.Session(ctx).Exec(`DELETE FROM wallet_ledger_entries`).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "append-only")

		err = dono.Session(ctx).Exec(`TRUNCATE wallet_ledger_entries`).Error
		require.Error(t, err, "TRUNCATE não passa por UPDATE nem DELETE e precisa de gatilho próprio")
		assert.Contains(t, err.Error(), "append-only")
	})

	t.Run("inserir continua permitido", func(t *testing.T) {
		outra := uuid.NewString()
		require.NoError(t, inserirExterna(app.Session(ctx), w, p, "WIN", "PENDING", 500, "BRL", "tx-2"))
		require.NoError(t, app.Session(ctx).Exec(
			`UPDATE wager_transactions SET id = ? WHERE external_transaction_id = 'tx-2'`, outra).Error)

		require.NoError(t, app.Session(ctx).Exec(`
			INSERT INTO wallet_ledger_entries
			  (id, wallet_id, transaction_id, direction, amount_minor, currency,
			   balance_before_minor, balance_after_minor)
			VALUES (?, ?, ?, 'CREDIT', 500, 'BRL', 2000, 2500)`,
			uuid.NewString(), w, outra).Error,
			"append-only não é somente leitura: correção financeira é lançamento novo")
	})
}

// O instantâneo do evento é imutável; o controle de publicação não é. Se o
// payload pudesse mudar depois, o evento publicado deixaria de descrever o que
// de fato aconteceu.
func TestInstantaneoDoEventoEhImutavel(t *testing.T) {
	dbCfg := migrado(t)
	app := abrir(t, dbCfg)
	ctx := context.Background()

	evento := uuid.NewString()
	require.NoError(t, app.Session(ctx).Exec(`
		INSERT INTO outbox_events
		  (event_id, event_type, aggregate_type, aggregate_id, version, payload, occurred_at)
		VALUES (?, 'WagerTransactionProcessed', 'wallet', ?, 1, '{"amount":"25.00"}', now())`,
		evento, uuid.NewString()).Error)

	t.Run("alterar o payload é recusado", func(t *testing.T) {
		err := app.Session(ctx).Exec(
			`UPDATE outbox_events SET payload = '{"amount":"9999.00"}' WHERE event_id = ?`,
			evento).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "imutavel")
	})

	t.Run("alterar o tipo do evento é recusado", func(t *testing.T) {
		err := app.Session(ctx).Exec(
			`UPDATE outbox_events SET event_type = 'Outro' WHERE event_id = ?`, evento).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "imutavel")
	})

	// Marcar como publicado é exatamente o que o worker precisa fazer.
	t.Run("o controle de publicação continua alterável", func(t *testing.T) {
		require.NoError(t, app.Session(ctx).Exec(`
			UPDATE outbox_events
			   SET published_at = now(), attempts = attempts + 1, locked_by = NULL, locked_until = NULL
			 WHERE event_id = ?`, evento).Error)
	})

	// A reivindicação é sempre um par: sem os dois campos não há como saber
	// que um lease expirou e o trabalho pode ser reassumido.
	t.Run("reivindicação pela metade é recusada", func(t *testing.T) {
		err := app.Session(ctx).Exec(
			`UPDATE outbox_events SET locked_by = 'instancia-1' WHERE event_id = ?`, evento).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outbox_events_lease_pairing")
	})
}

// aplicarMigrations leva um banco já de pé até a versão corrente do schema.
func aplicarMigrations(t *testing.T, dbCfg config.DB) {
	t.Helper()
	m := novoMigrator(t, asOwner(dbCfg))
	require.NoError(t, m.Up())
	require.NoError(t, m.Close())
}

// TestReversaoDaSextaPreservaODinheiroEDescartaSoAAnotacao prova a afirmação que
// a própria migration faz.
//
// A 000006 alarga o CHECK para o `WIN` poder carregar a referência da aposta da
// rodada, como o §7 permite. O schema anterior não sabe representar isso, então
// a reversão precisa anular o campo antes de reapertar a constraint — e a
// pergunta que importa é o que se perde nesse caminho.
//
// A resposta, e o que este teste trava: perde-se a ANOTAÇÃO, nunca o dinheiro. O
// tipo, o valor, o estado e o saldo resultante atravessam a reversão intactos.
// Se algum dia a referência do ganho passar a participar de um cálculo, este
// teste falha — e é essa a hora de a reversão deixar de ser possível.
func TestReversaoDaSextaPreservaODinheiroEDescartaSoAAnotacao(t *testing.T) {
	dbCfg := migrado(t)
	db := abrir(t, asOwner(dbCfg))
	m := novoMigrator(t, dbCfg)

	carteira, jogador := uuid.New(), uuid.New()
	require.NoError(t, db.Session(context.Background()).Exec(
		`INSERT INTO wallets (id, player_id, currency, balance_minor, version)
		 VALUES (?, ?, 'BRL', 10000, 1)`, carteira, jogador).Error)

	ganho := uuid.New()
	require.NoError(t, db.Session(context.Background()).Exec(
		`INSERT INTO wager_transactions
		   (id, kind, status, wallet_id, player_id, amount_minor, currency,
		    provider_id, external_transaction_id, idempotency_key, payload_hash,
		    round_id, game_id, reference_external_transaction_id,
		    result_balance_minor, processed_at)
		 VALUES (?, 'WIN', 'PROCESSED', ?, ?, 4500, 'BRL',
		    'provider-a', 'ganho-r1', 'provider-a:ganho-r1', '\x00',
		    'round-1', 'fortune-chimp', 'aposta-r1',
		    10000, now())`,
		ganho, carteira, jogador).Error)

	require.NoError(t, m.Down(), "a 000006 tem de ser reversível")

	var kind, status, moeda string
	var valor, saldo int64
	var referencia *string
	require.NoError(t, db.Session(context.Background()).Raw(
		`SELECT t.kind, t.status, t.currency, t.amount_minor,
		        t.reference_external_transaction_id, w.balance_minor
		   FROM wager_transactions t JOIN wallets w ON w.id = t.wallet_id
		  WHERE t.id = ?`, ganho).
		Row().Scan(&kind, &status, &moeda, &valor, &referencia, &saldo))

	assert.Nil(t, referencia, "a anotação some: o schema antigo não sabe guardá-la")
	assert.Equal(t, "WIN", kind)
	assert.Equal(t, "PROCESSED", status)
	assert.Equal(t, int64(4500), valor, "o valor atravessa a reversão intacto")
	assert.Equal(t, "BRL", moeda)
	assert.Equal(t, int64(10000), saldo, "e o saldo da carteira não é tocado")

	// E o schema antigo volta a recusar o que só a 000006 admite.
	err := db.Session(context.Background()).Exec(
		`UPDATE wager_transactions SET reference_external_transaction_id = 'aposta-r1'
		  WHERE id = ?`, ganho).Error
	require.Error(t, err, "revertida, a regra estrita volta a valer")
	assert.Contains(t, err.Error(), "wager_transactions_reference_by_kind")

	require.NoError(t, m.Up(), "e a ida continua funcionando depois da volta")
}
