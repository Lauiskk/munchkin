// Package postgres é o adaptador de persistência.
package postgres

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/Lauiskk/munchkin/pkg/logs"
)

// gormSlog encaminha o registro do GORM para o log estruturado da aplicação.
//
// A configuração mais importante aqui não está neste arquivo: é
// ParameterizedQueries, ligada em db.go. Sem ela o GORM interpola os valores
// dentro do SQL registrado — e num sistema financeiro isso põe valor monetário,
// identificador de jogador e chave de idempotência em texto claro no log, que é
// exatamente o que o enunciado proíbe. Com ela, o log mostra a consulta com
// placeholders e nenhum dado.
type gormSlog struct {
	log           *slog.Logger
	slowThreshold time.Duration
	level         gormlogger.LogLevel
}

func newGormLogger(log *slog.Logger, slowThreshold time.Duration, verbose bool) gormlogger.Interface {
	level := gormlogger.Warn
	if verbose {
		level = gormlogger.Info
	}
	return &gormSlog{log: log, slowThreshold: slowThreshold, level: level}
}

func (l *gormSlog) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	clone := *l
	clone.level = level
	return &clone
}

func (l *gormSlog) Info(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Info {
		l.log.LogAttrs(ctx, slog.LevelInfo, "db."+msg, slog.Any("data", data))
	}
}

func (l *gormSlog) Warn(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Warn {
		l.log.LogAttrs(ctx, slog.LevelWarn, "db."+msg, slog.Any("data", data))
	}
}

func (l *gormSlog) Error(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Error {
		l.log.LogAttrs(ctx, slog.LevelError, "db."+msg, slog.Any("data", data))
	}
}

// Trace é chamado ao fim de toda consulta.
func (l *gormSlog) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	sql, rows := fc()

	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		// Registro inexistente não é incidente: é resposta possível de uma
		// consulta. Tratá-lo como erro encheria o log de ruído e escoderia as
		// falhas de verdade.
		l.log.LogAttrs(ctx, slog.LevelError, "db.query_failed",
			slog.String("sql", sql),
			slog.Duration("elapsed", elapsed),
			slog.String(logs.KeyError, err.Error()))

	case l.slowThreshold > 0 && elapsed > l.slowThreshold:
		l.log.LogAttrs(ctx, slog.LevelWarn, "db.slow_query",
			slog.String("sql", sql),
			slog.Duration("elapsed", elapsed),
			slog.Int64("rows", rows),
			slog.Duration("threshold", l.slowThreshold))

	case l.level >= gormlogger.Info:
		l.log.LogAttrs(ctx, slog.LevelDebug, "db.query",
			slog.String("sql", sql),
			slog.Duration("elapsed", elapsed),
			slog.Int64("rows", rows))
	}
}
