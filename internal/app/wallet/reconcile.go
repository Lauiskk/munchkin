package wallet

import (
	"context"
	"log/slog"

	"github.com/Lauiskk/munchkin/internal/domain/money"
	domain "github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

// Reconciliation é o resultado da conferência entre saldo e ledger.
type Reconciliation struct {
	WalletID   domain.ID
	Stored     money.Money
	Calculated money.Money
	// Difference é o armazenado MENOS o reconstruído, como o §9 define. É o
	// único valor monetário do sistema que pode ser negativo numa resposta:
	// saldo menor que o ledger é tão divergência quanto o contrário, e esconder
	// o sinal esconderia metade dos casos.
	Difference     money.Money
	Consistent     bool
	CheckedEntries int64
}

// Snapshot é a leitura conjunta do saldo e do ledger.
type Snapshot struct {
	StoredMinor     int64
	CalculatedMinor int64
	Entries         int64
	Currency        money.Currency
}

// ReconcileReader lê saldo e ledger numa visão consistente.
type ReconcileReader interface {
	// Snapshot devolve o saldo armazenado, a soma dos lançamentos e quantos
	// foram somados — os três da MESMA visão dos dados.
	//
	// Ler em duas chamadas compararia o saldo de um instante com o ledger de
	// outro: uma aposta entre as duas leituras acusaria divergência onde não
	// há, e um alarme que dispara sozinho é pior que alarme nenhum, porque
	// ensina a ignorá-lo.
	Snapshot(ctx context.Context, id domain.ID) (Snapshot, error)
}

// Reconciler confere o saldo contra o ledger.
type Reconciler struct {
	reader ReconcileReader
	log    *slog.Logger
}

// NewReconciler monta a conferência.
func NewReconciler(reader ReconcileReader, log *slog.Logger) *Reconciler {
	return &Reconciler{reader: reader, log: log}
}

// Run confere e devolve o resultado. Não altera nada.
func (r *Reconciler) Run(ctx context.Context, id domain.ID) (Reconciliation, error) {
	visao, err := r.reader.Snapshot(ctx, id)
	if err != nil {
		return Reconciliation{}, err
	}

	armazenado, err := money.New(visao.StoredMinor, visao.Currency)
	if err != nil {
		return Reconciliation{}, err
	}
	reconstruido, err := money.New(visao.CalculatedMinor, visao.Currency)
	if err != nil {
		return Reconciliation{}, err
	}
	diferenca, err := armazenado.Sub(reconstruido)
	if err != nil {
		return Reconciliation{}, err
	}

	resultado := Reconciliation{
		WalletID:       id,
		Stored:         armazenado,
		Calculated:     reconstruido,
		Difference:     diferenca,
		Consistent:     diferenca.IsZero(),
		CheckedEntries: visao.Entries,
	}

	if !resultado.Consistent {
		// Nível de erro, e com os valores. Aqui os números SÃO o incidente: um
		// alerta de divergência sem dizer de quanto obriga quem for atender a
		// refazer a consulta à mão, no pior momento possível.
		r.log.LogAttrs(ctx, slog.LevelError, "wallet.reconciliation_divergent",
			slog.String(logs.KeyWalletID, id.String()),
			slog.String("storedBalance", armazenado.String()),
			slog.String("calculatedBalance", reconstruido.String()),
			slog.String("difference", diferenca.String()),
			slog.Int64("checkedEntries", visao.Entries))
	}
	return resultado, nil
}
