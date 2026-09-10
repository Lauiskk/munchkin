package dto

import (
	appledger "github.com/Lauiskk/munchkin/internal/app/ledger"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// LedgerEntryResponse é um lançamento no extrato.
type LedgerEntryResponse struct {
	ID            string      `json:"id"`
	TransactionID string      `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	// CreatedAt usa o mesmo formato dos instantes dos eventos: RFC 3339 UTC com
	// precisão fixa. Um sistema que emite datas em dois formatos obriga quem
	// integra a escrever dois parsers, e a errar um.
	CreatedAt string `json:"createdAt"`
}

// LedgerPageResponse é uma página do extrato.
type LedgerPageResponse struct {
	WalletID string                `json:"walletId"`
	Entries  []LedgerEntryResponse `json:"entries"`
	// NextCursor some na última página. É assim que o cliente sabe parar, sem
	// comparar o tamanho recebido com o limite pedido — comparação que erra
	// quando a última página vem cheia por coincidência.
	NextCursor string `json:"nextCursor,omitempty"`
}

// NewLedgerPageResponse monta a resposta do extrato.
func NewLedgerPageResponse(id domainwallet.ID, p appwallet.LedgerPage) LedgerPageResponse {
	entradas := make([]LedgerEntryResponse, 0, len(p.Entries))
	for _, e := range p.Entries {
		entradas = append(entradas, LedgerEntryResponse{
			ID:            e.ID().String(),
			TransactionID: e.TransactionID().String(),
			Direction:     string(e.Direction()),
			Money:         e.Amount(),
			BalanceBefore: e.BalanceBefore(),
			BalanceAfter:  e.BalanceAfter(),
			CreatedAt:     event.FormatTimestamp(e.CreatedAt()),
		})
	}

	resposta := LedgerPageResponse{WalletID: id.String(), Entries: entradas}
	if p.Next != nil {
		resposta.NextCursor = EncodeCursor(*p.Next)
	}
	return resposta
}

// ReconciliationResponse é o resultado da conferência.
type ReconciliationResponse struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	// Difference é o armazenado menos o reconstruído, e sai com sinal: saldo
	// menor que o ledger é tão divergência quanto o contrário.
	Difference     money.Money `json:"difference"`
	Consistent     bool        `json:"consistent"`
	CheckedEntries int64       `json:"checkedEntries"`
}

// NewReconciliationResponse monta a resposta da reconciliação.
func NewReconciliationResponse(r appwallet.Reconciliation) ReconciliationResponse {
	return ReconciliationResponse{
		WalletID:          r.WalletID.String(),
		StoredBalance:     r.Stored,
		CalculatedBalance: r.Calculated,
		Difference:        r.Difference,
		Consistent:        r.Consistent,
		CheckedEntries:    r.CheckedEntries,
	}
}

// AccountTotalResponse é o saldo acumulado de um tipo de conta.
type AccountTotalResponse struct {
	AccountKind string      `json:"accountKind"`
	Balance     money.Money `json:"balance"`
}

// CurrencyBalanceResponse é o balancete de uma moeda.
type CurrencyBalanceResponse struct {
	Currency string                 `json:"currency"`
	Accounts []AccountTotalResponse `json:"accounts"`
	// Total é a soma das contas da moeda, e tem de ser "0.00". Sai na resposta
	// em vez de ficar implícito no `balanced`: quem confere um balancete quer
	// ver o zero, não a promessa de que ele existe.
	Total money.Money `json:"total"`
}

// TrialBalanceResponse é o balancete das partidas dobradas.
type TrialBalanceResponse struct {
	Balanced        bool  `json:"balanced"`
	CheckedPostings int64 `json:"checkedPostings"`
	// UnbalancedTransactions só pode ser zero: o gatilho diferido recusa o
	// commit que a deixaria diferente disso. Sai na resposta como sintoma.
	UnbalancedTransactions int64                     `json:"unbalancedTransactions"`
	Currencies             []CurrencyBalanceResponse `json:"currencies"`
}

// NewTrialBalanceResponse monta a resposta do balancete.
func NewTrialBalanceResponse(b appledger.TrialBalance) TrialBalanceResponse {
	moedas := make([]CurrencyBalanceResponse, 0, len(b.Currencies))
	for _, m := range b.Currencies {
		contas := make([]AccountTotalResponse, 0, len(m.Accounts))
		for _, c := range m.Accounts {
			contas = append(contas, AccountTotalResponse{
				AccountKind: string(c.Kind),
				Balance:     c.Balance,
			})
		}
		moedas = append(moedas, CurrencyBalanceResponse{
			Currency: m.Currency.String(),
			Accounts: contas,
			Total:    m.Total,
		})
	}

	return TrialBalanceResponse{
		Balanced:               b.Balanced,
		CheckedPostings:        b.CheckedPostings,
		UnbalancedTransactions: b.UnbalancedTransactions,
		Currencies:             moedas,
	}
}
