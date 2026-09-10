// Package ledger reúne os casos de uso da contabilidade de partidas dobradas.
//
// Diferencial opcional do §6.4. O ledger que o enunciado especifica continua
// sendo o do pacote app/wallet — aqui mora só a leitura da contabilidade
// paralela, que nasce como projeção daquele.
package ledger

import (
	"context"
	"log/slog"

	domain "github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
)

// AccountSum é o total de um tipo de conta numa moeda, como o banco o devolve.
type AccountSum struct {
	Currency   money.Currency
	Kind       domain.AccountKind
	TotalMinor int64
	Postings   int64
}

// TrialBalanceSnapshot é a leitura crua, de uma visão só dos dados.
type TrialBalanceSnapshot struct {
	Sums []AccountSum
	// UnbalancedTransactions conta as transações cujas partidas não fecham.
	//
	// Só pode ser zero: o gatilho diferido recusa o commit que a deixaria
	// diferente disso. Existe como SINTOMA — se um dia subir, alguma escrita
	// alcançou a tabela por fora do caminho que o sistema conhece.
	UnbalancedTransactions int64
}

// PostingReader lê a contabilidade de partidas.
type PostingReader interface {
	// TrialBalance devolve os totais e a contagem de transações que não fecham,
	// os dois da MESMA visão dos dados.
	TrialBalance(ctx context.Context) (TrialBalanceSnapshot, error)
}

// AccountTotal é o saldo de um tipo de conta.
type AccountTotal struct {
	Kind    domain.AccountKind
	Balance money.Money
}

// CurrencyBalance é o balancete de uma moeda.
type CurrencyBalance struct {
	Currency money.Currency
	Accounts []AccountTotal
	// Total é a soma de todas as contas da moeda. Tem de ser zero: é isso que
	// "partidas dobradas" quer dizer.
	Total money.Money
}

// TrialBalance é o balancete: o que cada tipo de conta acumula, por moeda.
type TrialBalance struct {
	Balanced               bool
	CheckedPostings        int64
	UnbalancedTransactions int64
	Currencies             []CurrencyBalance
}

// TrialBalancer monta o balancete. Não altera nada.
type TrialBalancer struct {
	reader PostingReader
	log    *slog.Logger
}

// NewTrialBalancer monta o caso de uso.
func NewTrialBalancer(reader PostingReader, log *slog.Logger) *TrialBalancer {
	return &TrialBalancer{reader: reader, log: log}
}

// Run devolve o balancete.
//
// A soma é feita aqui, em unidades mínimas inteiras, e não no banco: a moeda
// precisa acompanhar o número para que somar BRL com USD seja impossível em vez
// de apenas errado.
func (t *TrialBalancer) Run(ctx context.Context) (TrialBalance, error) {
	visao, err := t.reader.TrialBalance(ctx)
	if err != nil {
		return TrialBalance{}, err
	}

	// A ordem das moedas segue a da consulta, que é ordenada: um balancete que
	// muda de ordem entre duas chamadas é impossível de comparar a olho.
	porMoeda := make(map[money.Currency]*CurrencyBalance, len(visao.Sums))
	ordem := make([]money.Currency, 0, len(visao.Sums))
	resultado := TrialBalance{UnbalancedTransactions: visao.UnbalancedTransactions}

	for _, s := range visao.Sums {
		valor, err := money.New(s.TotalMinor, s.Currency)
		if err != nil {
			return TrialBalance{}, err
		}

		moeda, existe := porMoeda[s.Currency]
		if !existe {
			zero, err := money.Zero(s.Currency)
			if err != nil {
				return TrialBalance{}, err
			}
			moeda = &CurrencyBalance{Currency: s.Currency, Total: zero}
			porMoeda[s.Currency] = moeda
			ordem = append(ordem, s.Currency)
		}

		moeda.Accounts = append(moeda.Accounts, AccountTotal{Kind: s.Kind, Balance: valor})
		if moeda.Total, err = moeda.Total.Add(valor); err != nil {
			return TrialBalance{}, err
		}
		resultado.CheckedPostings += s.Postings
	}

	resultado.Balanced = visao.UnbalancedTransactions == 0
	resultado.Currencies = make([]CurrencyBalance, 0, len(ordem))
	for _, c := range ordem {
		moeda := porMoeda[c]
		if !moeda.Total.IsZero() {
			resultado.Balanced = false
		}
		resultado.Currencies = append(resultado.Currencies, *moeda)
	}

	if !resultado.Balanced {
		// Nível de erro, e com os números: os livros não fecharem é o incidente
		// contábil, não um aviso. Um alerta que não diz de quanto obriga quem
		// atende a refazer a conta à mão, no pior momento possível.
		t.log.LogAttrs(ctx, slog.LevelError, "ledger.trial_balance_divergent",
			slog.Int64("unbalancedTransactions", resultado.UnbalancedTransactions),
			slog.Int64("checkedPostings", resultado.CheckedPostings),
			slog.String("totals", totaisEmTexto(resultado.Currencies)))
	}
	return resultado, nil
}

// totaisEmTexto resume as sobras por moeda numa linha de log.
func totaisEmTexto(moedas []CurrencyBalance) string {
	texto := ""
	for i, m := range moedas {
		if i > 0 {
			texto += ", "
		}
		texto += m.Total.String()
	}
	return texto
}
