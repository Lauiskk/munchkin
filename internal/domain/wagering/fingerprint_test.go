package wagering_test

import (
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

func impressao(t *testing.T) wagering.Fingerprint {
	t.Helper()
	_, wid, pid := partes(t)
	return wagering.Fingerprint{
		ProviderID: "provider-a",
		ExternalID: "transaction-123",
		PlayerID:   pid,
		WalletID:   wid,
		RoundID:    "round-987",
		GameID:     "fortune-chimp",
		Kind:       wagering.Bet,
		Amount:     brl(t, "25.00"),
	}
}

func hash(t *testing.T, f wagering.Fingerprint) string {
	t.Helper()
	h, err := f.Hash()
	require.NoError(t, err)
	return hex.EncodeToString(h)
}

// AC-E7: o hash é estável. Sem isso, o mesmo envio repetido pareceria conteúdo
// diferente e a idempotência não funcionaria.
func TestHashEhEstavelEntreChamadas(t *testing.T) {
	f := impressao(t)

	primeiro := hash(t, f)
	for i := 0; i < 20; i++ {
		assert.Equal(t, primeiro, hash(t, f))
	}
	assert.Len(t, primeiro, 64, "SHA-256 em hexadecimal")
}

// A ordenação das chaves vem do encoding/json, que serializa mapas ordenados.
// Duas requisições com os campos em ordens diferentes têm de dar o mesmo hash.
func TestCanonicoTemChavesOrdenadas(t *testing.T) {
	canonico := impressao(t).Canonical()

	var lido map[string]string
	require.NoError(t, json.Unmarshal([]byte(canonico), &lido))

	chaves := make([]string, 0, len(lido))
	for k := range lido {
		chaves = append(chaves, k)
	}
	require.NotEmpty(t, chaves)

	// Confere que a serialização saiu em ordem crescente de chave.
	anterior := ""
	for _, parte := range strings.Split(strings.Trim(canonico, "{}"), `","`) {
		chave := strings.SplitN(strings.TrimLeft(parte, `"`), `":`, 2)[0]
		assert.Less(t, anterior, chave, "as chaves precisam sair ordenadas")
		anterior = chave
	}
}

// Cada campo de negócio precisa mudar o hash. Um campo que não muda estaria
// fora do cálculo — e duas operações diferentes seriam tratadas como a mesma.
func TestCadaCampoDeNegocioAlteraOHash(t *testing.T) {
	base := impressao(t)
	original := hash(t, base)

	outraCarteira, err := wallet.NewID()
	require.NoError(t, err)
	outroJogador, err := wallet.NewPlayerID()
	require.NoError(t, err)

	variacoes := map[string]func(f *wagering.Fingerprint){
		"provedor":   func(f *wagering.Fingerprint) { f.ProviderID = "provider-b" },
		"id externo": func(f *wagering.Fingerprint) { f.ExternalID = "transaction-999" },
		"jogador":    func(f *wagering.Fingerprint) { f.PlayerID = outroJogador },
		"carteira":   func(f *wagering.Fingerprint) { f.WalletID = outraCarteira },
		"rodada":     func(f *wagering.Fingerprint) { f.RoundID = "round-000" },
		"jogo":       func(f *wagering.Fingerprint) { f.GameID = "outro-jogo" },
		"tipo":       func(f *wagering.Fingerprint) { f.Kind = wagering.Win },
		"valor":      func(f *wagering.Fingerprint) { f.Amount = brl(t, "25.01") },
		"referência": func(f *wagering.Fingerprint) { f.ReferenceExternalID = "tx-1" },
	}
	for nome, alterar := range variacoes {
		t.Run(nome, func(t *testing.T) {
			variante := base
			alterar(&variante)
			assert.NotEqual(t, original, hash(t, variante),
				"mudar %s tem de mudar o hash", nome)
		})
	}
}

// A moeda faz parte da identidade: "25.00 BRL" e "25.00 USD" são operações
// diferentes, e um hash igual as trataria como a mesma.
func TestMoedaDiferenteProduzHashDiferente(t *testing.T) {
	base := impressao(t)
	emDolar := base
	dolares, err := money.Parse("25.00", money.USD)
	require.NoError(t, err)
	emDolar.Amount = dolares

	assert.NotEqual(t, hash(t, base), hash(t, emDolar))
}

// A referência aparece SEMPRE no canônico, vazia quando não se aplica. Incluí-la
// condicionalmente faria o mesmo conteúdo produzir hashes diferentes conforme o
// campo estivesse presente ou ausente no corpo recebido.
func TestConjuntoDeCamposEhFixo(t *testing.T) {
	semReferencia := impressao(t)

	var lido map[string]string
	require.NoError(t, json.Unmarshal([]byte(semReferencia.Canonical()), &lido))

	assert.Contains(t, lido, "referenceExternalTransactionId")
	assert.Empty(t, lido["referenceExternalTransactionId"])

	// Afirma o conjunto EXATO, não a contagem: acrescentar um campo ao hash
	// muda a identidade de toda operação já registrada, e isso não pode passar
	// despercebido numa revisão.
	chaves := make([]string, 0, len(lido))
	for k := range lido {
		chaves = append(chaves, k)
	}
	sort.Strings(chaves)
	assert.Equal(t, []string{
		"externalTransactionId",
		"gameId",
		"kind",
		"money.amount",
		"money.currency",
		"playerId",
		"providerId",
		"referenceExternalTransactionId",
		"roundId",
		"walletId",
	}, chaves, "o conjunto de campos do hash é contrato")
}

// O que NÃO está no hash importa tanto quanto o que está: instante, estado e
// identificador interno mudam entre um envio e o reenvio da mesma operação.
func TestHashNaoDependeDeMetadadosDeTransporte(t *testing.T) {
	canonico := impressao(t).Canonical()

	for _, ausente := range []string{
		"idempotencyKey", "receivedAt", "status", "transactionId",
		"attempts", "correlationId", "messageId",
	} {
		assert.NotContains(t, canonico, ausente,
			"%s não pode entrar no hash: muda entre reenvios da mesma operação", ausente)
	}
}

func TestValorNaoInicializadoImpedeOHash(t *testing.T) {
	f := impressao(t)
	f.Amount = money.Money{}

	_, err := f.Hash()
	require.Error(t, err)
	assert.ErrorIs(t, err, money.ErrUninitialized)
}

func TestChaveDeIdempotencia(t *testing.T) {
	t.Run("aceita o formato usual", func(t *testing.T) {
		k, err := wagering.ParseIdempotencyKey("provider-a:transaction-123")
		require.NoError(t, err)
		assert.Equal(t, "provider-a:transaction-123", k)
	})

	t.Run("recusa entrada inválida", func(t *testing.T) {
		invalidas := map[string]string{
			"vazia":           "",
			"espaço na borda": " chave",
			"quebra de linha": "chave\ninjetada",
			"aspas":           `chave"forjada`,
			"longa demais":    strings.Repeat("k", 129),
		}
		for nome, valor := range invalidas {
			t.Run(nome, func(t *testing.T) {
				_, err := wagering.ParseIdempotencyKey(valor)
				assert.ErrorIs(t, err, wagering.ErrInvalidIdempotencyKey)
			})
		}
	})
}
