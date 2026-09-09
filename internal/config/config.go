// Package config carrega a configuração a partir do ambiente e a valida antes
// de a aplicação subir.
//
// A validação acumula todos os problemas e falha uma vez só, listando tudo.
// Falhar no primeiro erro obrigaria a subir, corrigir, subir de novo, a cada
// variável — o que é exatamente o que ninguém tem paciência de fazer.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// ErrInvalid marca falha de configuração, permitindo distingui-la com
// errors.Is de qualquer outra falha na subida.
var ErrInvalid = errors.New("configuração inválida")

// Config é a configuração completa da aplicação.
type Config struct {
	App  App
	HTTP HTTP
	Log  Log
	Auth Auth
	DB   DB
}

// App identifica o ambiente de execução.
type App struct {
	Env string
}

// IsProduction informa se estamos em produção, o que endurece alguns padrões.
func (a App) IsProduction() bool { return a.Env == EnvProduction }

// Ambientes reconhecidos.
const (
	EnvLocal      = "local"
	EnvTest       = "test"
	EnvProduction = "production"
)

// HTTP reúne os parâmetros do servidor.
type HTTP struct {
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
}

// Addr devolve o endereço de escuta.
func (h HTTP) Addr() string { return fmt.Sprintf(":%d", h.Port) }

// DB reúne os parâmetros de conexão com o PostgreSQL.
type DB struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password Secret
	SSLMode  string

	// MaxOpenConns limita as conexões simultâneas por instância. O padrão é
	// baixo de propósito: o enunciado exige demonstrar as garantias com pelo
	// menos três processos independentes, e três instâncias generosas
	// esgotariam o max_connections padrão do PostgreSQL antes de qualquer
	// teste começar.
	MaxOpenConns int
	MaxIdleConns int
	// ConnMaxLifetime evita conexões eternas, que envelhecem mal atrás de
	// balanceadores e após failover.
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	// ConnectTimeout limita a espera na abertura de conexão.
	ConnectTimeout time.Duration
}

// DSN monta a string de conexão. Não há método String no DB justamente para
// que ninguém imprima a struct inteira achando que está tudo redigido.
func (d DB) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=%d",
		d.Host, d.Port, d.Name, d.User, d.Password.Reveal(), d.SSLMode,
		int(d.ConnectTimeout.Seconds()),
	)
}

// URL monta a string de conexão no formato URL, esperado pelo executor de
// migrations. A senha é escapada: uma senha com "@" ou "/" quebraria a URL de
// forma silenciosa, e o erro apareceria como host inexistente.
func (d DB) URL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(d.User, d.Password.Reveal()),
		Host:   net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path:   "/" + d.Name,
	}
	q := url.Values{}
	q.Set("sslmode", d.SSLMode)
	q.Set("connect_timeout", strconv.Itoa(int(d.ConnectTimeout.Seconds())))
	u.RawQuery = q.Encode()
	return u.String()
}

// Auth reúne os parâmetros de validação de token.
type Auth struct {
	// Issuer é o valor esperado na claim `iss`.
	Issuer string
	// DiscoveryURL é de onde o documento OIDC é buscado. Pode diferir do
	// Issuer: dentro da rede do compose a aplicação alcança o IdP por um
	// endereço interno, enquanto o token continua sendo emitido com o
	// endereço público.
	DiscoveryURL string
	// Audience é o valor esperado na claim `aud`.
	Audience string
	// JWKSRefreshInterval é o período do ticker de atualização das chaves.
	JWKSRefreshInterval time.Duration
	// JWKSMinRefreshInterval limita a frequência da atualização forçada por
	// `kid` desconhecido, para que tokens forjados não virem enxurrada de
	// requisições ao IdP.
	JWKSMinRefreshInterval time.Duration
	// HTTPTimeout limita cada chamada ao IdP.
	HTTPTimeout time.Duration
}

// Log reúne os parâmetros de registro.
type Log struct {
	Level  string
	Format string
}

// Formatos de log aceitos.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Load lê o ambiente, valida e devolve a configuração.
//
// Um arquivo .env presente no diretório de trabalho é carregado antes, para
// conveniência local. Ele nunca sobrescreve variável já definida no ambiente,
// então container e CI continuam mandando no que vale.
func Load() (Config, error) {
	_ = godotenv.Load() // ausência de .env é o caso normal fora do desenvolvimento

	var v validator

	cfg := Config{
		App: App{
			Env: v.enum("APP_ENV", EnvLocal, EnvLocal, EnvTest, EnvProduction),
		},
		HTTP: HTTP{
			Port:            v.port("HTTP_PORT", 8080),
			ReadTimeout:     v.duration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    v.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
			RequestTimeout:  v.duration("HTTP_REQUEST_TIMEOUT", 10*time.Second),
			ShutdownTimeout: v.duration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		},
		Log: Log{
			Level:  v.enum("LOG_LEVEL", "info", "debug", "info", "warn", "error"),
			Format: v.enum("LOG_FORMAT", LogFormatJSON, LogFormatJSON, LogFormatText),
		},
		Auth: Auth{
			// Emissor e audiência são obrigatórios. Um serviço que sobe sem
			// saber qual IdP confiar aceitaria tráfego sem conseguir validar
			// token — e ausência de autenticação efetiva nos endpoints de
			// negócio é eliminatória.
			Issuer:                 v.required("AUTH_ISSUER"),
			Audience:               v.required("AUTH_AUDIENCE"),
			DiscoveryURL:           v.optional("AUTH_DISCOVERY_URL"),
			JWKSRefreshInterval:    v.duration("AUTH_JWKS_REFRESH_INTERVAL", 15*time.Minute),
			JWKSMinRefreshInterval: v.duration("AUTH_JWKS_MIN_REFRESH_INTERVAL", time.Minute),
			HTTPTimeout:            v.duration("AUTH_HTTP_TIMEOUT", 5*time.Second),
		},
		DB: loadDB(&v),
	}

	// O prazo de uma requisição precisa caber na janela de escrita, senão o
	// servidor corta a resposta antes de o handler desistir e o cliente recebe
	// uma conexão encerrada em vez do erro de timeout.
	if cfg.HTTP.RequestTimeout >= cfg.HTTP.WriteTimeout {
		v.fail("HTTP_REQUEST_TIMEOUT (%s) deve ser menor que HTTP_WRITE_TIMEOUT (%s)",
			cfg.HTTP.RequestTimeout, cfg.HTTP.WriteTimeout)
	}

	// Mais conexões ociosas que abertas é configuração sem sentido: o pool
	// nunca alcançaria o limite de ociosas, e quem configurou provavelmente
	// trocou os dois valores de lugar.
	if cfg.DB.MaxIdleConns > cfg.DB.MaxOpenConns {
		v.fail("DB_MAX_IDLE_CONNS (%d) não pode exceder DB_MAX_OPEN_CONNS (%d)",
			cfg.DB.MaxIdleConns, cfg.DB.MaxOpenConns)
	}

	if err := v.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadDB carrega e valida apenas a configuração de banco.
//
// Existe para o executor de migrations, que precisa do banco e de nada mais.
// Fazê-lo passar por Load obrigaria o serviço de migração a receber emissor e
// audiência do IdP — credenciais que ele não usa e não deveria carregar. Menos
// configuração exigida é menos segredo distribuído.
func LoadDB() (DB, error) {
	_ = godotenv.Load()

	var v validator
	db := loadDB(&v)
	if err := v.err(); err != nil {
		return DB{}, err
	}
	return db, nil
}

// loadDB monta a configuração de banco acumulando problemas no validador.
func loadDB(v *validator) DB {
	return DB{
		Host:            v.required("DB_HOST"),
		Port:            v.port("DB_PORT", 5432),
		Name:            v.required("DB_NAME"),
		User:            v.required("DB_USER"),
		Password:        Secret(v.required("DB_PASSWORD")),
		SSLMode:         v.enum("DB_SSLMODE", "disable", "disable", "require", "verify-ca", "verify-full"),
		MaxOpenConns:    v.positiveInt("DB_MAX_OPEN_CONNS", 10),
		MaxIdleConns:    v.positiveInt("DB_MAX_IDLE_CONNS", 5),
		ConnMaxLifetime: v.duration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		ConnMaxIdleTime: v.duration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute),
		ConnectTimeout:  v.duration("DB_CONNECT_TIMEOUT", 5*time.Second),
	}
}

// validator acumula problemas de configuração para reportar todos de uma vez.
type validator struct {
	problems []string
}

func (v *validator) fail(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  - %s", ErrInvalid,
		strings.Join(v.problems, "\n  - "))
}

// required lê uma variável obrigatória.
func (v *validator) required(key string) string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		v.fail("%s: obrigatória e não definida", key)
	}
	return raw
}

// optional lê uma variável que pode estar ausente.
func (v *validator) optional(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// enum lê uma variável restrita a um conjunto de valores.
func (v *validator) enum(key, fallback string, allowed ...string) string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	for _, a := range allowed {
		if raw == a {
			return raw
		}
	}
	v.fail("%s: %q não é aceito; use um de: %s", key, raw, strings.Join(allowed, ", "))
	return fallback
}

// port lê uma porta TCP.
func (v *validator) port(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		v.fail("%s: %q não é um número inteiro", key, raw)
		return fallback
	}
	if n < 1 || n > 65535 {
		v.fail("%s: %d fora da faixa de portas válidas (1-65535)", key, n)
		return fallback
	}
	return n
}

// positiveInt lê um inteiro que precisa ser maior que zero.
func (v *validator) positiveInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		v.fail("%s: %q não é um número inteiro", key, raw)
		return fallback
	}
	if n < 1 {
		v.fail("%s: %d deve ser maior que zero", key, n)
		return fallback
	}
	return n
}

// duration lê uma duração no formato aceito por time.ParseDuration.
func (v *validator) duration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		v.fail("%s: %q não é uma duração válida (exemplos: 15s, 500ms, 2m)", key, raw)
		return fallback
	}
	if d <= 0 {
		v.fail("%s: %s deve ser maior que zero", key, d)
		return fallback
	}
	return d
}
