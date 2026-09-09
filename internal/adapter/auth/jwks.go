// Package auth valida os tokens emitidos pelo IdP externo.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Lauiskk/munchkin/pkg/logs"
)

// Limites da busca do JWKS. O corpo é limitado porque um endpoint comprometido
// — ou apenas mal configurado — poderia devolver uma resposta enorme e derrubar
// o processo por consumo de memória durante o que deveria ser uma manutenção
// de rotina.
const (
	maxJWKSBytes    = 1 << 20 // 1 MiB
	minRSAKeyBits   = 2048
	discoverySuffix = "/.well-known/openid-configuration"
)

var (
	// ErrKeyNotFound indica assinatura por chave que o IdP não publica.
	ErrKeyNotFound = errors.New("chave de assinatura não encontrada no JWKS")
	// ErrIssuerMismatch indica que o IdP apontado não é o esperado.
	ErrIssuerMismatch = errors.New("o emissor publicado pelo IdP difere do configurado")
)

// KeySet mantém em memória as chaves públicas do IdP.
//
// A intenção é eliminar a ida na rede a cada requisição, não a verificação: a
// assinatura continua sendo conferida em toda requisição, localmente, com a
// chave já carregada. O que some é a viagem até o Keycloak, não a checagem.
//
// A atualização acontece em três momentos:
//
//   - na subida, e uma falha aqui impede a aplicação de subir. Um serviço que
//     aceita tráfego sem conseguir validar token é pior que um serviço fora do ar;
//   - periodicamente, por ticker, para acompanhar rotação programada;
//   - sob demanda, quando chega um token assinado por um `kid` desconhecido —
//     que é exatamente como uma rotação não programada se manifesta. Essa
//     atualização tem intervalo mínimo, senão uma enxurrada de tokens forjados
//     com `kid` aleatório viraria uma enxurrada de requisições ao IdP.
type KeySet struct {
	issuer       string
	discoveryURL string
	httpClient   *http.Client
	log          *slog.Logger

	refreshInterval    time.Duration
	minRefreshInterval time.Duration

	mu          sync.RWMutex
	jwksURI     string
	keys        map[string]*rsa.PublicKey
	lastRefresh time.Time

	// refreshMu serializa as atualizações forçadas. Sem ela, mil requisições
	// simultâneas com o mesmo `kid` novo dispararia mil buscas ao IdP.
	refreshMu sync.Mutex
}

// KeySetOptions reúne o que o KeySet precisa para funcionar.
type KeySetOptions struct {
	// Issuer é o valor esperado na claim `iss`. É o que validamos.
	Issuer string
	// DiscoveryURL é onde o documento de descoberta é buscado. Pode diferir do
	// Issuer: dentro da rede do compose a aplicação alcança o IdP por um
	// endereço interno, enquanto o token continua sendo emitido com o endereço
	// público. Vazio significa Issuer + /.well-known/openid-configuration.
	DiscoveryURL       string
	RefreshInterval    time.Duration
	MinRefreshInterval time.Duration
	HTTPTimeout        time.Duration
	Logger             *slog.Logger
}

// NewKeySet monta o conjunto de chaves. Nada de rede acontece aqui — a primeira
// busca é em Bootstrap, para que a falha aconteça no ciclo de vida, onde ela
// pode impedir a subida.
func NewKeySet(opts KeySetOptions) *KeySet {
	discovery := opts.DiscoveryURL
	if discovery == "" {
		discovery = strings.TrimRight(opts.Issuer, "/") + discoverySuffix
	}
	return &KeySet{
		issuer:             opts.Issuer,
		discoveryURL:       discovery,
		httpClient:         &http.Client{Timeout: opts.HTTPTimeout},
		log:                opts.Logger,
		refreshInterval:    opts.RefreshInterval,
		minRefreshInterval: opts.MinRefreshInterval,
		keys:               map[string]*rsa.PublicKey{},
	}
}

// Bootstrap descobre o endereço do JWKS e carrega as chaves pela primeira vez.
func (k *KeySet) Bootstrap(ctx context.Context) error {
	jwksURI, err := k.discover(ctx)
	if err != nil {
		return fmt.Errorf("descoberta OIDC em %s: %w", k.discoveryURL, err)
	}

	k.mu.Lock()
	k.jwksURI = jwksURI
	k.mu.Unlock()

	if err := k.Refresh(ctx); err != nil {
		return fmt.Errorf("carga inicial do JWKS: %w", err)
	}
	return nil
}

// discoveryDocument é o recorte do documento de descoberta que nos interessa.
type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

func (k *KeySet) discover(ctx context.Context) (string, error) {
	var doc discoveryDocument
	if err := k.fetchJSON(ctx, k.discoveryURL, &doc); err != nil {
		return "", err
	}

	// Conferir o emissor publicado contra o configurado impede que uma URL de
	// descoberta trocada por engano — outro realm, outro ambiente — passe
	// despercebida. Sem esta checagem, o serviço validaria felizmente tokens
	// de um IdP que não é o dele.
	if doc.Issuer != k.issuer {
		return "", fmt.Errorf("%w: publicado %q, configurado %q",
			ErrIssuerMismatch, doc.Issuer, k.issuer)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("documento de descoberta sem jwks_uri")
	}
	return doc.JWKSURI, nil
}

// Refresh busca o JWKS e substitui as chaves em memória.
func (k *KeySet) Refresh(ctx context.Context) error {
	k.mu.RLock()
	uri := k.jwksURI
	k.mu.RUnlock()

	if uri == "" {
		return errors.New("JWKS ainda não descoberto")
	}

	var set jwkSet
	if err := k.fetchJSON(ctx, uri, &set); err != nil {
		return err
	}

	keys, ignoradas := set.rsaKeys()
	if len(keys) == 0 {
		// Substituir por um conjunto vazio deixaria o serviço rejeitando todo
		// token válido. Preservar as chaves atuais é mais seguro: elas ainda
		// funcionam até expirarem.
		return errors.New("JWKS não trouxe nenhuma chave RSA utilizável")
	}

	k.mu.Lock()
	k.keys = keys
	k.lastRefresh = time.Now()
	k.mu.Unlock()

	k.log.LogAttrs(ctx, slog.LevelInfo, "auth.jwks_refreshed",
		slog.Int("keys", len(keys)),
		slog.Int("ignored", ignoradas))
	return nil
}

// KeyFor devolve a chave pública do `kid`, atualizando o JWKS quando ele for
// desconhecido — que é como uma rotação de chave chega até nós.
func (k *KeySet) KeyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if key, ok := k.lookup(kid); ok {
		return key, nil
	}

	if err := k.refreshForUnknownKID(ctx, kid); err != nil {
		return nil, err
	}

	if key, ok := k.lookup(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: kid %q", ErrKeyNotFound, kid)
}

func (k *KeySet) lookup(kid string) (*rsa.PublicKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	key, ok := k.keys[kid]
	return key, ok
}

// refreshForUnknownKID atualiza o JWKS respeitando o intervalo mínimo.
func (k *KeySet) refreshForUnknownKID(ctx context.Context, kid string) error {
	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()

	// Outra goroutine pode ter atualizado enquanto esperávamos a trava.
	if _, ok := k.lookup(kid); ok {
		return nil
	}

	k.mu.RLock()
	since := time.Since(k.lastRefresh)
	k.mu.RUnlock()

	if since < k.minRefreshInterval {
		// Recusar aqui é proteção contra um cliente que force o serviço a
		// martelar o IdP mandando tokens com `kid` aleatório.
		return fmt.Errorf("%w: kid %q, atualização recente demais (%s)",
			ErrKeyNotFound, kid, since.Truncate(time.Millisecond))
	}

	k.log.LogAttrs(ctx, slog.LevelInfo, "auth.jwks_refresh_forced",
		slog.String("kid", kid))
	return k.Refresh(ctx)
}

// RefreshInterval expõe o período do ticker.
func (k *KeySet) RefreshInterval() time.Duration { return k.refreshInterval }

func (k *KeySet) fetchJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			k.log.LogAttrs(ctx, slog.LevelWarn, "auth.body_close_failed",
				slog.String(logs.KeyError, cerr.Error()))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("resposta %d de %s", resp.StatusCode, url)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(dst)
}

// jwkSet é o JWKS publicado pelo IdP.
type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// rsaKeys converte o conjunto, descartando o que não serve para verificar
// assinatura RS256. Devolve também quantas chaves foram ignoradas, porque um
// JWKS inteiro ignorado é sintoma de configuração errada e precisa aparecer.
func (s jwkSet) rsaKeys() (map[string]*rsa.PublicKey, int) {
	keys := make(map[string]*rsa.PublicKey, len(s.Keys))
	ignored := 0

	for _, j := range s.Keys {
		if j.Kty != "RSA" || j.Kid == "" {
			ignored++
			continue
		}
		// O Keycloak publica chaves de assinatura e de criptografia no mesmo
		// conjunto. Usar uma chave de criptografia para verificar assinatura é
		// erro de uso de chave, então filtramos por alg e use.
		if j.Use != "" && j.Use != "sig" {
			ignored++
			continue
		}
		if j.Alg != "" && j.Alg != "RS256" {
			ignored++
			continue
		}

		key, err := j.rsaPublicKey()
		if err != nil {
			ignored++
			continue
		}
		keys[j.Kid] = key
	}
	return keys, ignored
}

func (j jwk) rsaPublicKey() (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil {
		return nil, fmt.Errorf("modulo inválido: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(j.E)
	if err != nil {
		return nil, fmt.Errorf("expoente inválido: %w", err)
	}
	if len(eBytes) == 0 || len(eBytes) > 8 {
		return nil, errors.New("expoente fora do tamanho esperado")
	}

	n := new(big.Int).SetBytes(nBytes)
	if n.BitLen() < minRSAKeyBits {
		// Uma chave curta demais é fraca. Aceitá-la seria aceitar assinatura
		// que pode ser forjada com esforço viável.
		return nil, fmt.Errorf("chave RSA de %d bits, mínimo %d", n.BitLen(), minRSAKeyBits)
	}

	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e < 2 {
		return nil, errors.New("expoente inválido")
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}
