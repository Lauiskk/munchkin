package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	appledger "github.com/Lauiskk/munchkin/internal/app/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
)

// appendPostings grava o par de partidas derivado de um lançamento.
//
// Vive junto do Append do ledger, e não como uma porta que o caso de uso teria
// de lembrar de chamar. A razão é a mesma que faz das partidas uma projeção: se
// existissem duas chamadas, existiria um caminho que faz a primeira e esquece a
// segunda — e o ledger passaria a discordar da contabilidade. Assim não existe
// forma de gravar um lançamento sem gravar o par.
//
// As duas linhas vão numa instrução só. Não é economia de ida ao banco: um par
// escrito em dois comandos teria um instante em que os livros não fecham, e o
// gatilho do banco existe justamente para o caso de alguém escrever assim.
// Depender do diferimento é pior que não precisar dele.
func (r *LedgerRepository) appendPostings(ctx context.Context, e ledger.Entry) error {
	par, err := ledger.DoubleEntry(e)
	if err != nil {
		return err
	}

	args := make([]any, 0, 14)
	for _, p := range par {
		args = append(args, argumentosDaPartida(p)...)
	}

	err = r.db.Session(ctx).Exec(`
		INSERT INTO ledger_postings
		  (id, transaction_id, account_kind, wallet_id, currency, amount_minor, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?)`, args...).Error

	return classify(err)
}

// argumentosDaPartida devolve a linha de uma partida na ordem do INSERT.
func argumentosDaPartida(p ledger.Posting) []any {
	// A conta da casa não pertence a carteira nenhuma, e o banco cobra isso: o
	// CHECK amarra o tipo da conta à presença do identificador. Um zero no lugar
	// do nulo passaria a chave estrangeira a apontar para uma carteira que não
	// existe.
	var carteira any
	if p.Account().Kind() == ledger.WalletAccount {
		carteira = uuid.UUID(p.Account().WalletID())
	}

	return []any{
		uuid.UUID(p.ID()), uuid.UUID(p.TransactionID()),
		string(p.Account().Kind()), carteira,
		p.Account().Currency().String(),
		p.Amount().Minor(), p.CreatedAt(),
	}
}

// PostingRepository lê a contabilidade de partidas dobradas.
//
// Só lê. Quem escreve é o Append do ledger, porque partida é projeção de
// lançamento — não há origem independente.
type PostingRepository struct{ db *Database }

// NewPostingRepository monta o repositório de leitura das partidas.
func NewPostingRepository(db *Database) *PostingRepository { return &PostingRepository{db: db} }

// TrialBalance devolve os totais por moeda e tipo de conta, e quantas
// transações não fecham.
//
// Numa instrução só, pelo mesmo motivo da reconciliação: dois SELECTs
// comparariam os totais de um instante com a contagem de outro, e uma operação
// entre as duas leituras acusaria divergência onde não há. Alarme que dispara
// sozinho é pior que alarme nenhum, porque ensina a ignorá-lo.
func (r *PostingRepository) TrialBalance(
	ctx context.Context,
) (appledger.TrialBalanceSnapshot, error) {
	var linhas []struct {
		Currency   string `gorm:"column:currency"`
		Kind       string `gorm:"column:account_kind"`
		TotalMinor int64  `gorm:"column:total_minor"`
		Postings   int64  `gorm:"column:postings"`
		Unbalanced int64  `gorm:"column:unbalanced"`
	}

	err := r.db.Session(ctx).Raw(`
		WITH por_conta AS (
		     SELECT currency, account_kind,
		            SUM(amount_minor) AS total_minor,
		            COUNT(*)          AS postings
		       FROM ledger_postings
		      GROUP BY currency, account_kind
		), quebradas AS (
		     SELECT COUNT(*) AS n FROM (
		            SELECT transaction_id
		              FROM ledger_postings
		             GROUP BY transaction_id
		            HAVING SUM(amount_minor) <> 0
		                OR COUNT(*) < 2
		                OR COUNT(DISTINCT currency) <> 1
		     ) q
		)
		SELECT p.currency, p.account_kind, p.total_minor, p.postings,
		       q.n AS unbalanced
		  FROM por_conta p CROSS JOIN quebradas q
		 ORDER BY p.currency, p.account_kind`).Scan(&linhas).Error
	if err != nil {
		return appledger.TrialBalanceSnapshot{}, classify(err)
	}

	visao := appledger.TrialBalanceSnapshot{Sums: make([]appledger.AccountSum, 0, len(linhas))}
	for _, l := range linhas {
		moeda, err := money.ParseCurrency(l.Currency)
		if err != nil {
			return appledger.TrialBalanceSnapshot{}, err
		}
		tipo := ledger.AccountKind(l.Kind)
		if tipo != ledger.WalletAccount && tipo != ledger.HouseAccount {
			return appledger.TrialBalanceSnapshot{},
				fmt.Errorf("%w: tipo de conta %q", ledger.ErrInvalidAccount, l.Kind)
		}
		visao.Sums = append(visao.Sums, appledger.AccountSum{
			Currency:   moeda,
			Kind:       tipo,
			TotalMinor: l.TotalMinor,
			Postings:   l.Postings,
		})
		// O valor repete em toda linha porque a consulta é uma só — é o preço
		// de ler tudo da mesma visão dos dados, e é barato.
		visao.UnbalancedTransactions = l.Unbalanced
	}
	return visao, nil
}
