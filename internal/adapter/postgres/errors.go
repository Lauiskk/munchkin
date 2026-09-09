package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Códigos SQLSTATE que nos interessam.
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateCheckViolation      = "23514"
	sqlstateForeignKeyViolation = "23503"
	sqlstateRestrictViolation   = "2F004" // levantado pelos gatilhos de imutabilidade
)

var (
	// ErrConflict indica violação de unicidade: o registro já existe.
	ErrConflict = errors.New("registro já existe")
	// ErrConstraintViolated indica violação de uma invariante do schema.
	//
	// Chegar aqui significa que o domínio deixou passar algo que o banco
	// recusou — ou seja, um defeito nosso. É erro de 500, não de 4xx, e precisa
	// aparecer no log com o nome da constraint.
	ErrConstraintViolated = errors.New("invariante do banco violada")
	// ErrImmutable indica tentativa de alterar registro append-only.
	ErrImmutable = errors.New("registro imutável")
)

// ConstraintError carrega o nome da constraint violada.
//
// O nome importa: "violação de unicidade" não diz se foi carteira duplicada ou
// operação repetida, e o caso de uso responde coisas diferentes para cada uma.
type ConstraintError struct {
	Constraint string
	Kind       error // ErrConflict, ErrConstraintViolated ou ErrImmutable
	cause      error
}

func (e *ConstraintError) Error() string {
	return fmt.Sprintf("%v: %s", e.Kind, e.Constraint)
}

func (e *ConstraintError) Unwrap() error { return e.Kind }

// Cause devolve o erro original do driver, para o log.
func (e *ConstraintError) Cause() error { return e.cause }

// classify converte um erro do driver em erro tipado com o nome da constraint.
//
// Sem isto, o caso de uso teria de comparar texto de mensagem do PostgreSQL —
// que muda entre versões e locales, e transforma uma regra de negócio numa
// dependência da língua do servidor.
func classify(err error) error {
	if err == nil {
		return nil
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	var kind error
	switch pgErr.Code {
	case sqlstateUniqueViolation:
		kind = ErrConflict
	case sqlstateCheckViolation, sqlstateForeignKeyViolation:
		kind = ErrConstraintViolated
	case sqlstateRestrictViolation:
		kind = ErrImmutable
	default:
		return err
	}

	nome := pgErr.ConstraintName
	if nome == "" {
		nome = pgErr.TableName
	}
	return &ConstraintError{Constraint: nome, Kind: kind, cause: err}
}

// ConstraintNameOf devolve o nome da constraint violada, se houver.
func ConstraintNameOf(err error) (string, bool) {
	var ce *ConstraintError
	if errors.As(err, &ce) {
		return ce.Constraint, true
	}
	return "", false
}

// IsConstraint informa se o erro é violação da constraint indicada.
//
// É assim que o caso de uso distingue "carteira já existe para este jogador" de
// "esta operação já foi registrada", sem consultar antes de inserir — a consulta
// prévia seria uma corrida: entre o SELECT e o INSERT, outro processo insere.
func IsConstraint(err error, nome string) bool {
	got, ok := ConstraintNameOf(err)
	return ok && got == nome
}

// Nomes das constraints que o código consulta. Ficam aqui, em constante, para
// que uma renomeação em migration quebre a compilação em vez de virar um erro
// silenciosamente não reconhecido.
const (
	ConstraintWalletPlayerCurrency = "wallets_player_currency_uk"
	ConstraintOpeningPerWallet     = "wager_transactions_opening_uk"
	ConstraintProviderExternalID   = "wager_transactions_provider_external_uk"
	ConstraintProviderIdempotency  = "wager_transactions_provider_key_uk"
	ConstraintSingleReversal       = "wager_transactions_single_reversal_uk"
	ConstraintLedgerPerTransaction = "wallet_ledger_entries_wallet_transaction_uk"
	ConstraintInboxConsumerMessage = "inbox_messages_consumer_message_uk"
)
