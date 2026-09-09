package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

// runMigrate executa o subcomando de migrations e devolve o código de saída.
//
// É subcomando do mesmo binário porque as migrations estão embarcadas nele:
// não há como aplicar uma versão de schema que não corresponda ao código que
// vai usá-la. No ambiente, quem roda isto é um serviço separado, com as
// credenciais do DONO do schema — a aplicação nunca as recebe.
func runMigrate(args []string) int {
	if len(args) == 0 {
		printMigrateUsage()
		return 2
	}

	// Só a configuração de banco: o executor de migrations não precisa de
	// emissor nem audiência do IdP, e não deve carregar credencial que não usa.
	dbCfg, err := config.LoadDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	log := logs.New(config.Config{Log: config.Log{Level: "info", Format: config.LogFormatJSON}})

	migrator, err := postgres.NewMigrator(dbCfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	defer func() {
		if cerr := migrator.Close(); cerr != nil {
			log.Warn("migrate.close_failed", slog.String(logs.KeyError, cerr.Error()))
		}
	}()

	if err := dispatchMigrate(migrator, args); err != nil {
		if errors.Is(err, postgres.ErrDirty) {
			fmt.Fprintln(os.Stderr,
				"o banco está marcado como sujo: uma migração foi interrompida no meio.\n"+
					"confira o schema à mão e, depois de decidir qual versão vale,\n"+
					"execute: api migrate force <versao>")
			return 1
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return 0
}

func dispatchMigrate(m *postgres.Migrator, args []string) error {
	switch args[0] {
	case "up":
		return m.Up()

	case "down":
		return m.Down()

	case "down-all":
		// Reverter tudo apaga os dados. Exigir a confirmação explícita evita
		// que um "down" digitado com pressa leve o schema inteiro junto.
		if os.Getenv("MIGRATE_CONFIRM_DESTRUCTIVE") != "yes" {
			return errors.New(
				"down-all reverte todas as migrations e apaga os dados.\n" +
					"para confirmar: MIGRATE_CONFIRM_DESTRUCTIVE=yes api migrate down-all")
		}
		return m.DownAll()

	case "force":
		if len(args) < 2 {
			return errors.New("uso: api migrate force <versao>")
		}
		version, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("versão inválida: %q", args[1])
		}
		return m.Force(version)

	case "status":
		version, dirty, err := m.Version()
		if err != nil {
			return err
		}
		estado := "limpo"
		if dirty {
			estado = "SUJO — migração interrompida"
		}
		fmt.Printf("versao=%d estado=%s\n", version, estado)
		return nil

	default:
		printMigrateUsage()
		return fmt.Errorf("subcomando desconhecido: %q", args[0])
	}
}

func printMigrateUsage() {
	fmt.Fprintln(os.Stderr, `uso: api migrate <comando>

  up            aplica todas as migrations pendentes
  down          reverte uma migration
  down-all      reverte todas (destrutivo; exige MIGRATE_CONFIRM_DESTRUCTIVE=yes)
  status        mostra a versao corrente e se o banco esta sujo
  force <n>     marca a versao n como aplicada e limpa o estado sujo`)
}
