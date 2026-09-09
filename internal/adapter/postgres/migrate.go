package postgres

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // driver do destino
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/migrations"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

// ErrDirty indica migração interrompida no meio, deixando o banco num estado
// que o executor não sabe classificar.
var ErrDirty = errors.New("o banco está marcado como sujo por uma migração interrompida")

// Migrator aplica e reverte as migrations versionadas.
//
// As migrations rodam como DONO do schema, nunca com o papel da aplicação —
// que, de propósito, não tem permissão para criar nem alterar estrutura.
type Migrator struct {
	m   *migrate.Migrate
	log *slog.Logger
}

// NewMigrator monta o executor sobre as migrations embarcadas no binário.
func NewMigrator(dbCfg config.DB, log *slog.Logger) (*Migrator, error) {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("leitura das migrations embarcadas: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, dbCfg.URL())
	if err != nil {
		// O erro do driver pode carregar a URL inteira, e com ela a senha.
		return nil, fmt.Errorf("conexão do executor de migrations com %s:%d/%s falhou",
			dbCfg.Host, dbCfg.Port, dbCfg.Name)
	}

	return &Migrator{m: m, log: log}, nil
}

// Up aplica todas as migrations pendentes.
func (mg *Migrator) Up() error {
	if err := mg.guardClean(); err != nil {
		return err
	}
	if err := mg.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return mg.report("migrate.up")
}

// Down reverte uma migração.
//
// Reverter uma de cada vez é o padrão de propósito: reverter tudo apaga os
// dados, e essa não é a operação que alguém quer por engano ao digitar rápido.
func (mg *Migrator) Down() error {
	if err := mg.guardClean(); err != nil {
		return err
	}
	if err := mg.m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return mg.report("migrate.down")
}

// DownAll reverte todas as migrations, destruindo o schema e os dados.
func (mg *Migrator) DownAll() error {
	if err := mg.guardClean(); err != nil {
		return err
	}
	if err := mg.m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return mg.report("migrate.down_all")
}

// Force marca a versão indicada como aplicada e limpa o estado sujo. É
// reparo manual, e só faz sentido depois de conferir o schema à mão.
func (mg *Migrator) Force(version int) error {
	if err := mg.m.Force(version); err != nil {
		return err
	}
	return mg.report("migrate.forced")
}

// Version devolve a versão corrente e se o banco está sujo.
func (mg *Migrator) Version() (uint, bool, error) {
	version, dirty, err := mg.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return version, dirty, err
}

// Close libera as conexões do executor.
func (mg *Migrator) Close() error {
	sourceErr, dbErr := mg.m.Close()
	return errors.Join(sourceErr, dbErr)
}

// guardClean recusa operar sobre um banco sujo.
//
// Aplicar migração por cima de um estado que o executor não sabe classificar é
// como se produz um schema meio aplicado que ninguém consegue reproduzir.
func (mg *Migrator) guardClean() error {
	_, dirty, err := mg.Version()
	if err != nil {
		return err
	}
	if dirty {
		return ErrDirty
	}
	return nil
}

func (mg *Migrator) report(event string) error {
	version, dirty, err := mg.Version()
	if err != nil {
		mg.log.Error(event, slog.String(logs.KeyError, err.Error()))
		return err
	}
	mg.log.Info(event, slog.Uint64("version", uint64(version)), slog.Bool("dirty", dirty))
	return nil
}
