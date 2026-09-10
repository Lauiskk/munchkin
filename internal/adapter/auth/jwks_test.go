package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
)

// idpFalso serve um documento de descoberta e um JWKS controlados pelo teste.
type idpFalso struct {
	*httptest.Server
	// issuerPublicado é o `issuer` do documento; pode divergir do endereço real
	// do servidor, que é justamente o caso que se quer exercitar.
	issuerPublicado  string
	jwksURIPublicado string
	buscasAoJWKS     int
	devolverVazio    bool
}

func novoIdPFalso(t *testing.T) *idpFalso {
	t.Helper()
	idp := &idpFalso{}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   idp.issuerPublicado,
			"jwks_uri": idp.jwksURIPublicado,
		})
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		idp.buscasAoJWKS++
		if idp.devolverVazio {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"keys":[]}`)
			return
		}
		chave, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "kid": "chave-1", "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(chave.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(chave.E)).Bytes()),
			}},
		})
	})

	idp.Server = httptest.NewServer(mux)
	idp.issuerPublicado = idp.URL
	idp.jwksURIPublicado = idp.URL + "/certs"
	t.Cleanup(idp.Close)
	return idp
}

func opcoes(issuer, discovery string) auth.KeySetOptions {
	return auth.KeySetOptions{
		Issuer:             issuer,
		DiscoveryURL:       discovery,
		RefreshInterval:    time.Minute,
		MinRefreshInterval: 0,
		HTTPTimeout:        3 * time.Second,
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

// O IdP monta o jwks_uri a partir do seu endereço público, que não resolve de
// dentro da rede do compose. Quando uma URL de descoberta interna é
// configurada, o JWKS precisa ser buscado pelo mesmo caminho interno.
func TestJWKSEhBuscadoPeloMesmoCaminhoDaDescoberta(t *testing.T) {
	const publico = "http://endereco-publico-que-nao-resolve:9999"

	// Reproduz a topologia do compose: o IdP publica o endereço público no
	// documento, mas a aplicação só o alcança pelo endereço interno.
	idp := novoIdPFalso(t)
	idp.issuerPublicado = publico
	idp.jwksURIPublicado = publico + "/certs"

	ks := auth.NewKeySet(opcoes(publico, idp.URL+"/.well-known/openid-configuration"))

	require.NoError(t, ks.Bootstrap(context.Background()),
		"o jwks_uri publicado não resolve; o endereço da descoberta precisa prevalecer")
	assert.Equal(t, 1, idp.buscasAoJWKS, "o JWKS foi buscado pelo caminho interno")
}

// Sem URL de descoberta configurada não há topologia interna, e o jwks_uri
// publicado é seguido como está.
func TestSemDescobertaConfiguradaOJWKSPublicadoEhSeguido(t *testing.T) {
	idp := novoIdPFalso(t)
	ks := auth.NewKeySet(opcoes(idp.URL, ""))

	require.NoError(t, ks.Bootstrap(context.Background()))
	assert.Equal(t, 1, idp.buscasAoJWKS)
}

func TestEmissorDivergenteImpedeASubida(t *testing.T) {
	idp := novoIdPFalso(t)
	idp.issuerPublicado = "http://outro-emissor"

	ks := auth.NewKeySet(opcoes(idp.URL, ""))

	err := ks.Bootstrap(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrIssuerMismatch)
	assert.Equal(t, 0, idp.buscasAoJWKS, "não se busca chave de um IdP que não é o nosso")
}

func TestChaveDesconhecidaForcaUmaAtualizacaoSo(t *testing.T) {
	idp := novoIdPFalso(t)
	ks := auth.NewKeySet(opcoes(idp.URL, ""))
	require.NoError(t, ks.Bootstrap(context.Background()))
	require.Equal(t, 1, idp.buscasAoJWKS)

	_, err := ks.KeyFor(context.Background(), "kid-inexistente")

	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrKeyNotFound)
	assert.Equal(t, 2, idp.buscasAoJWKS, "uma atualização forçada, não mais")
}

// Sem intervalo mínimo, tokens com kid aleatório virariam enxurrada contra o IdP.
func TestIntervaloMinimoBarraAtualizacoesSeguidas(t *testing.T) {
	idp := novoIdPFalso(t)
	op := opcoes(idp.URL, "")
	op.MinRefreshInterval = time.Hour
	ks := auth.NewKeySet(op)
	require.NoError(t, ks.Bootstrap(context.Background()))

	for i := 0; i < 5; i++ {
		_, err := ks.KeyFor(context.Background(), "kid-aleatorio")
		require.Error(t, err)
	}
	assert.Equal(t, 1, idp.buscasAoJWKS,
		"apenas a carga inicial; as forçadas foram barradas pelo intervalo mínimo")
}

// Um JWKS sem chave utilizável não pode zerar o conjunto em memória: as chaves
// atuais ainda funcionam, e substituí-las por nada deixaria o serviço
// recusando todo token válido.
func TestJWKSVazioNaoDescartaAsChavesAtuais(t *testing.T) {
	idp := novoIdPFalso(t)
	ks := auth.NewKeySet(opcoes(idp.URL, ""))
	require.NoError(t, ks.Bootstrap(context.Background()))

	_, err := ks.KeyFor(context.Background(), "chave-1")
	require.NoError(t, err, "a chave carregada na subida precisa estar disponível")

	// A partir daqui o IdP passa a devolver um conjunto vazio.
	idp.devolverVazio = true

	err = ks.Refresh(context.Background())
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "nenhuma chave rsa")

	// E o essencial: a chave anterior continua servindo. Zerar o conjunto
	// deixaria o serviço recusando todo token válido por causa de uma
	// atualização malsucedida.
	_, err = ks.KeyFor(context.Background(), "chave-1")
	assert.NoError(t, err, "uma atualização malsucedida não pode apagar o que funciona")
}

// TestIdPForaDoArDepoisDaSubidaNaoDerrubaAAutenticacao guarda a propriedade
// que dá razão de existir ao cache de chaves.
//
// Achado numa passada de QA como lacuna: derrubar o IdP com a aplicação de pé
// foi verificado à mão contra a pilha viva — e a API continuou autenticando —,
// mas nenhum teste o exercitava. Havia teste para o IdP fora do ar na SUBIDA
// (falha rápido, correto) e para o JWKS devolvido vazio, que é um IdP saudável
// respondendo mal. Faltava o caso do meio, que é o que acontece de verdade em
// produção: o IdP cai depois, e o tráfego já autenticado não pode parar.
//
// Se isto regredir, uma indisponibilidade do IdP vira 401 em todo o tráfego —
// uma dependência de leitura derrubando o caminho financeiro inteiro.
func TestIdPForaDoArDepoisDaSubidaNaoDerrubaAAutenticacao(t *testing.T) {
	idp := novoIdPFalso(t)
	ks := auth.NewKeySet(opcoes(idp.URL, ""))
	require.NoError(t, ks.Bootstrap(context.Background()))

	_, err := ks.KeyFor(context.Background(), "chave-1")
	require.NoError(t, err)
	buscasAteAqui := idp.buscasAoJWKS

	// O IdP some. Não responde devagar, não responde errado: some.
	idp.Close()

	_, err = ks.KeyFor(context.Background(), "chave-1")
	assert.NoError(t, err, "a chave em memória não depende de o IdP estar de pé")

	// A atualização periódica falha, e é ela que roda enquanto o IdP está fora.
	err = ks.Refresh(context.Background())
	require.Error(t, err, "sem IdP não há o que buscar")

	// E o que importa: depois da falha, o conjunto continua servindo. Uma
	// atualização malsucedida não pode ter efeito pior que não ter acontecido.
	_, err = ks.KeyFor(context.Background(), "chave-1")
	assert.NoError(t, err, "a falha de atualização não pode apagar o que funciona")

	// E um kid desconhecido continua RECUSADO. O erro aqui é o de rede, e não
	// ErrKeyNotFound — a atualização forçada nem chega a ler resposta —, mas o
	// que importa para a segurança é que ele é um erro: o middleware traduz
	// qualquer falha do verificador em 401, então o IdP fora do ar não abre
	// brecha para token com chave desconhecida. Falha fechado, que é o único
	// desfecho aceitável numa verificação de credencial.
	_, err = ks.KeyFor(context.Background(), "kid-que-nunca-existiu")
	require.Error(t, err, "sem chave e sem IdP, a única resposta segura é recusar")
	assert.Equal(t, buscasAteAqui, idp.buscasAoJWKS,
		"o servidor está fora: nenhuma busca chegou a ser atendida")
}
