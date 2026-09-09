package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/Lauiskk/munchkin/internal/config"
)

// slowQueryThreshold marca a consulta que merece atenção. No caminho financeiro
// a transação segura o lock da carteira enquanto executa, então consulta lenta
// ali não é só latência: é contenção sobre a carteira.
const slowQueryThreshold = 200 * time.Millisecond

// Database é o acesso ao PostgreSQL.
//
// Expõe duas coisas e nada mais: como obter o handle correto para a operação
// atual (Session) e como delimitar uma transação (Within). Repositórios
// dependem deste tipo, nunca de um *gorm.DB solto — foi assim que o projeto de
// referência da equipe acabou com um singleton global e nenhuma fronteira
// transacional visível.
type Database struct {
	pool *gorm.DB
	log  *slog.Logger
}

// Open abre a conexão e ajusta o pool.
func Open(cfg config.Config, log *slog.Logger) (*Database, error) {
	gormCfg := &gorm.Config{
		Logger: newGormLogger(log, slowQueryThreshold, cfg.Log.Level == "debug"),

		// O GORM embrulha cada Create e Update solto numa transação própria.
		// Desligar isso não é otimização: é manter visível quem abre e quem
		// fecha transação. Com o padrão ligado, uma escrita fora do nosso
		// bloco transacional commitaria sozinha, sem ninguém perceber.
		SkipDefaultTransaction: true,

		// Traduz erro de driver para erro do GORM (chave duplicada, violação
		// de checagem), o que permite classificar com errors.Is em vez de
		// comparar texto de mensagem do PostgreSQL.
		TranslateError: true,

		// Todo instante gravado é UTC. Sem isto, o fuso da máquina entra nos
		// dados e duas instâncias em fusos diferentes gravariam instantes
		// incomparáveis para a mesma operação.
		NowFunc: func() time.Time { return time.Now().UTC() },
	}

	db, err := gorm.Open(postgres.Open(cfg.DB.DSN()), gormCfg)
	if err != nil {
		// O erro do driver pode conter a string de conexão inteira, e com ela
		// a senha. Substituímos por uma mensagem própria.
		return nil, fmt.Errorf("abertura da conexão com o PostgreSQL falhou (host=%s port=%d db=%s user=%s)",
			cfg.DB.Host, cfg.DB.Port, cfg.DB.Name, cfg.DB.User)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("obtenção do pool: %w", err)
	}
	tunePool(sqlDB, cfg.DB)

	return &Database{pool: db, log: log}, nil
}

// tunePool aplica os limites do pool.
//
// O projeto de referência da equipe não configurava nenhum destes, e o padrão
// do database/sql é conexões abertas ilimitadas: sob carga, a aplicação abre
// conexão até o PostgreSQL recusar, e aí o erro aparece como indisponibilidade
// do banco em vez de esgotamento de pool.
func tunePool(sqlDB *sql.DB, cfg config.DB) {
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
}

// Session devolve o handle a usar na operação atual: a transação corrente, se
// houver uma no context, ou o pool.
//
// É este método que permite ao repositório não saber se está dentro de uma
// transação. A fronteira transacional fica no caso de uso, onde é legível, em
// vez de espalhada por quem escreve no banco.
func (d *Database) Session(ctx context.Context) *gorm.DB {
	if tx := txFromContext(ctx); tx != nil {
		return tx
	}
	return d.pool.WithContext(ctx)
}

// Ping verifica que o banco responde.
func (d *Database) Ping(ctx context.Context) error {
	sqlDB, err := d.pool.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

// Stats expõe o estado do pool, para métrica e diagnóstico.
func (d *Database) Stats() sql.DBStats {
	sqlDB, err := d.pool.DB()
	if err != nil {
		return sql.DBStats{}
	}
	return sqlDB.Stats()
}

// Close encerra o pool.
func (d *Database) Close() error {
	sqlDB, err := d.pool.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
