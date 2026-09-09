package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"github.com/Lauiskk/munchkin/pkg/logs"
)

// ErrNoTransaction indica uso de uma operação que exige transação aberta fora
// de uma. Existe para que esse engano falhe alto, em vez de escrever no pool.
var ErrNoTransaction = errors.New("operação exige transação aberta")

type txCtxKey struct{}

func txFromContext(ctx context.Context) *gorm.DB {
	tx, _ := ctx.Value(txCtxKey{}).(*gorm.DB)
	return tx
}

func txIntoContext(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txCtxKey{}, tx)
}

// InTransaction informa se o context já carrega uma transação.
func InTransaction(ctx context.Context) bool { return txFromContext(ctx) != nil }

// Within executa fn dentro de uma transação, confirmando ao final e desfazendo
// em caso de erro ou pânico.
//
// A transação é propagada pelo context, então o repositório chamado lá dentro
// enxerga o mesmo handle sem que ninguém precise passá-lo de mão em mão — e sem
// que ele possa escrever fora dela por engano.
//
// Chamada aninhada NÃO abre uma segunda transação: ela reaproveita a corrente.
// Abrir outra quebraria a atomicidade de forma silenciosa, que é o pior jeito
// de quebrá-la — o estado da operação, o saldo, o lançamento do ledger e os
// eventos precisam ser confirmados juntos ou não serem confirmados.
func (d *Database) Within(ctx context.Context, fn func(context.Context) error) error {
	if InTransaction(ctx) {
		return fn(ctx)
	}

	tx := d.pool.WithContext(ctx).Begin()
	if tx.Error != nil {
		// Passa pela classificação como qualquer outro erro do banco. Sem isso,
		// um banco fora do ar falha AQUI — antes de qualquer repositório — e o
		// erro chegaria ao cliente como defeito da aplicação em vez de
		// "indisponível, tente de novo".
		return fmt.Errorf("abertura de transação: %w", classify(tx.Error))
	}

	// O pânico precisa desfazer a transação antes de subir. Sem isto, a
	// conexão volta ao pool com uma transação aberta e a próxima operação a
	// pegar essa conexão herda o estado sujo.
	//
	// E o pânico é repropagado: engoli-lo aqui converteria um defeito de
	// programação em erro de negócio silencioso.
	defer func() {
		if r := recover(); r != nil {
			d.rollback(ctx, tx, "panic")
			panic(r)
		}
	}()

	if err := fn(txIntoContext(ctx, tx)); err != nil {
		d.rollback(ctx, tx, "error")
		return err
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("confirmação da transação: %w", classify(err))
	}
	return nil
}

// rollback desfaz a transação e registra falha no desfazimento.
//
// Uma falha aqui não substitui o erro original — o chamador precisa saber o que
// deu errado no negócio, não que o desfazimento também falhou. Mas ela precisa
// aparecer, porque significa conexão possivelmente inutilizável.
func (d *Database) rollback(ctx context.Context, tx *gorm.DB, motivo string) {
	if err := tx.Rollback().Error; err != nil && !errors.Is(err, gorm.ErrInvalidTransaction) {
		d.log.LogAttrs(ctx, slog.LevelError, "db.rollback_failed",
			slog.String("reason", motivo),
			slog.String(logs.KeyError, err.Error()))
	}
}
