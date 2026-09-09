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
	}

	// O prazo de uma requisição precisa caber na janela de escrita, senão o
	// servidor corta a resposta antes de o handler desistir e o cliente recebe
	// uma conexão encerrada em vez do erro de timeout.
	if cfg.HTTP.RequestTimeout >= cfg.HTTP.WriteTimeout {
		v.fail("HTTP_REQUEST_TIMEOUT (%s) deve ser menor que HTTP_WRITE_TIMEOUT (%s)",
			cfg.HTTP.RequestTimeout, cfg.HTTP.WriteTimeout)
	}

	if err := v.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
