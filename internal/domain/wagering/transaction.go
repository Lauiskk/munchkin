// Package wagering implementa a operação financeira e sua máquina de estados.
package wagering

import (
	"errors"
	"fmt"
	"time"

	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

var (
	// ErrUninitialized indica agregado que nunca passou por construtor.
	ErrUninitialized = errors.New("transação não inicializada")
	// ErrInvalidAmountForKind indica valor incompatível com o tipo.
	ErrInvalidAmountForKind = errors.New("valor incompatível com o tipo da operação")
	// ErrMissingReference indica reversão sem a referência obrigatória.
	ErrMissingReference = errors.New("reversão exige referência")
	// ErrUnexpectedReference indica referência informada onde não cabe.
	ErrUnexpectedReference = errors.New("apenas reversões aceitam referência")
	// ErrMissingOrigin indica operação externa sem os metadados obrigatórios.
	ErrMissingOrigin = errors.New("operação externa exige metadados de origem")
)

// ExternalOrigin reúne os metadados de uma operação vinda de provedor.
//
// É um bloco só porque os campos vivem e morrem juntos: ou a operação é externa
// e tem todos, ou é interna e não tem nenhum. Campos soltos permitiriam a
// combinação incoerente que o schema recusa.
type ExternalOrigin struct {
	ProviderID     ProviderID
	ExternalID     ExternalID
	IdempotencyKey string
	// PayloadHash é o SHA-256 do JSON canônico dos campos de negócio.
	PayloadHash []byte
	RoundID     RoundID
	GameID      GameID
}

func (o ExternalOrigin) validate() error {
	if o.ProviderID == "" || o.ExternalID == "" || o.IdempotencyKey == "" ||
		len(o.PayloadHash) == 0 || o.RoundID == "" || o.GameID == "" {
		return ErrMissingOrigin
	}
	return nil
}

// Transaction é a operação financeira.
//
// Estado encapsulado: o status só muda pelos métodos de transição, que consultam
// a máquina de estados. Não há caminho para escrever um status direto.
type Transaction struct {
	id     TransactionID
	kind   Kind
	status Status

	walletID wallet.ID
	playerID wallet.PlayerID
	amount   money.Money

	// origin é nil para operações internas.
	origin *ExternalOrigin

	referenceExternalID ExternalID
	referenceID         *TransactionID

	failureCode   FailureCode
	resultBalance *money.Money

	attempts      int
	nextAttemptAt *time.Time

	createdAt   time.Time
	updatedAt   time.Time
	processedAt *time.Time
}

// NewOpening cria a transação de abertura interna de carteira.
//
// Não tem provedor, identificador externo, chave, hash, rodada nem jogo: esses
// metadados não se aplicam a uma origem interna, e o schema recusa a linha que
// os traga.
func NewOpening(
	id TransactionID, walletID wallet.ID, playerID wallet.PlayerID,
	amount money.Money, now time.Time,
) (*Transaction, error) {
	t, err := newBase(id, Opening, walletID, playerID, amount, now)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// NewExternal cria uma operação vinda de provedor.
func NewExternal(
	id TransactionID, kind Kind, walletID wallet.ID, playerID wallet.PlayerID,
	amount money.Money, origin ExternalOrigin, referenceExternalID ExternalID,
	now time.Time,
) (*Transaction, error) {
	if kind.IsInternal() {
		return nil, fmt.Errorf("%w: %s é reservado à abertura interna", ErrInvalidKind, kind)
	}
	if err := origin.validate(); err != nil {
		return nil, err
	}

	// Três regras, não duas: reversão EXIGE referência, WIN PODE ter, e o resto
	// não pode. A terceira direção é a que protege o resolvedor — sem ela uma
	// aposta com referência passaria e alguém tentaria resolvê-la.
	//
	// O WIN é a diferença: o §7 permite que ele informe a aposta da mesma
	// rodada. Ela fica gravada e nada mais — quem resolve e quem espera são os
	// guardas de MarkPendingReference e ResolveReference, ambos presos a
	// IsReversal, e é por isso que aceitar a referência aqui não faz um ganho
	// virar pendência.
	switch {
	case kind.IsReversal() && referenceExternalID == "":
		return nil, fmt.Errorf("%w: %s", ErrMissingReference, kind)
	case !kind.AcceptsReference() && referenceExternalID != "":
		return nil, fmt.Errorf("%w: %s", ErrUnexpectedReference, kind)
	}

	t, err := newBase(id, kind, walletID, playerID, amount, now)
	if err != nil {
		return nil, err
	}
	t.origin = &origin
	t.referenceExternalID = referenceExternalID
	return t, nil
}

// newBase aplica o que é comum às duas origens.
func newBase(
	id TransactionID, kind Kind, walletID wallet.ID, playerID wallet.PlayerID,
	amount money.Money, now time.Time,
) (*Transaction, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: identificador de transação ausente", ErrInvalidID)
	}
	if walletID.IsZero() {
		return nil, fmt.Errorf("%w: identificador de carteira ausente", ErrInvalidID)
	}
	if playerID.IsZero() {
		return nil, fmt.Errorf("%w: identificador de jogador ausente", ErrInvalidID)
	}
	if _, ok := kindsValidos[kind]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, kind)
	}
	if !amount.IsValid() {
		return nil, fmt.Errorf("%w: valor ausente", money.ErrUninitialized)
	}
	if err := checkAmountForKind(kind, amount); err != nil {
		return nil, err
	}

	now = now.UTC()
	return &Transaction{
		id:        id,
		kind:      kind,
		status:    Pending,
		walletID:  walletID,
		playerID:  playerID,
		amount:    amount,
		createdAt: now,
		updatedAt: now,
	}, nil
}

// checkAmountForKind aplica a política de valor de cada tipo.
func checkAmountForKind(kind Kind, amount money.Money) error {
	if kind.RequiresZeroAmount() {
		if !amount.IsZero() {
			return fmt.Errorf("%w: %s exige valor zero, recebeu %s",
				ErrInvalidAmountForKind, kind, amount)
		}
		return nil
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: %s exige valor positivo, recebeu %s",
			ErrInvalidAmountForKind, kind, amount)
	}
	return nil
}

// Rehydrate reconstrói a transação a partir do estado persistido.
//
// Não valida a máquina de estados nem reaplica nada: o estado persistido já é o
// resultado de transições que aconteceram. Validar de novo aqui recusaria linhas
// legítimas se a máquina mudasse depois.
func Rehydrate(s State) (*Transaction, error) {
	if s.ID.IsZero() {
		return nil, fmt.Errorf("%w: identificador de transação ausente", ErrInvalidID)
	}
	// A validação usa os mesmos parsers da entrada externa. O estado veio do
	// banco, que tem CHECK sobre tipo e estado — mas o código de falha não tem,
	// e um código desconhecido devolvido ao provedor quebraria a integração
	// dele sem nenhum erro do nosso lado.
	if _, err := ParseKind(string(s.Kind)); err != nil {
		return nil, err
	}
	if _, err := ParseStatus(string(s.Status)); err != nil {
		return nil, err
	}
	if s.FailureCode != "" {
		if _, err := ParseFailureCode(string(s.FailureCode)); err != nil {
			return nil, err
		}
	}
	if !s.Amount.IsValid() {
		return nil, fmt.Errorf("%w: valor ausente", money.ErrUninitialized)
	}

	return &Transaction{
		id: s.ID, kind: s.Kind, status: s.Status,
		walletID: s.WalletID, playerID: s.PlayerID, amount: s.Amount,
		origin:              s.Origin,
		referenceExternalID: s.ReferenceExternalID,
		referenceID:         s.ReferenceID,
		failureCode:         s.FailureCode,
		resultBalance:       s.ResultBalance,
		attempts:            s.Attempts,
		nextAttemptAt:       s.NextAttemptAt,
		createdAt:           s.CreatedAt.UTC(),
		updatedAt:           s.UpdatedAt.UTC(),
		processedAt:         s.ProcessedAt,
	}, nil
}

// State é o retrato do estado persistido, usado na reidratação.
//
// Existe para que Rehydrate não tenha quinze parâmetros posicionais, onde trocar
// dois de lugar compila sem reclamar.
type State struct {
	ID                  TransactionID
	Kind                Kind
	Status              Status
	WalletID            wallet.ID
	PlayerID            wallet.PlayerID
	Amount              money.Money
	Origin              *ExternalOrigin
	ReferenceExternalID ExternalID
	ReferenceID         *TransactionID
	FailureCode         FailureCode
	ResultBalance       *money.Money
	Attempts            int
	NextAttemptAt       *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ProcessedAt         *time.Time
}

// transitionTo aplica a máquina de estados.
func (t *Transaction) transitionTo(destino Status, now time.Time) error {
	if t == nil || t.status == "" {
		return ErrUninitialized
	}
	if t.status.IsTerminal() {
		return fmt.Errorf("%w: %s", ErrTerminal, t.status)
	}
	if !t.status.CanTransitionTo(destino) {
		return fmt.Errorf("%w: %s para %s", ErrInvalidTransition, t.status, destino)
	}
	t.status = destino
	t.updatedAt = now.UTC()
	return nil
}

// MarkProcessed conclui a operação com sucesso.
//
// O saldo resultante é guardado porque é ele que o replay devolve — mesmo que a
// carteira já tenha se movimentado depois, quem reenviar a mesma operação
// recebe o saldo observado no processamento original.
func (t *Transaction) MarkProcessed(resultBalance money.Money, now time.Time) error {
	if !resultBalance.IsValid() {
		return fmt.Errorf("%w: saldo resultante ausente", money.ErrUninitialized)
	}
	if err := t.transitionTo(Processed, now); err != nil {
		return err
	}
	at := now.UTC()
	t.resultBalance = &resultBalance
	t.processedAt = &at
	return nil
}

// MarkRejected recusa a operação por regra de negócio.
func (t *Transaction) MarkRejected(code FailureCode, now time.Time) error {
	if _, ok := failureCodesValidos[code]; !ok {
		return fmt.Errorf("código de falha desconhecido: %q", code)
	}
	if err := t.transitionTo(Rejected, now); err != nil {
		return err
	}
	t.failureCode = code
	return nil
}

// MarkFailed registra falha permanente de infraestrutura, para auditoria.
func (t *Transaction) MarkFailed(code FailureCode, now time.Time) error {
	if _, ok := failureCodesValidos[code]; !ok {
		return fmt.Errorf("código de falha desconhecido: %q", code)
	}
	if err := t.transitionTo(Failed, now); err != nil {
		return err
	}
	t.failureCode = code
	return nil
}

// MarkPendingReference registra a espera por uma referência ainda indisponível.
func (t *Transaction) MarkPendingReference(nextAttemptAt, now time.Time) error {
	if !t.kind.IsReversal() {
		return fmt.Errorf("%w: %s não depende de referência", ErrUnexpectedReference, t.kind)
	}
	if err := t.transitionTo(PendingReference, now); err != nil {
		return err
	}
	at := nextAttemptAt.UTC()
	t.nextAttemptAt = &at
	return nil
}

// ScheduleRetry agenda nova tentativa de resolver a referência.
func (t *Transaction) ScheduleRetry(nextAttemptAt, now time.Time) error {
	if t == nil || t.status != PendingReference {
		return fmt.Errorf("%w: retentativa exige estado %s", ErrInvalidTransition, PendingReference)
	}
	at := nextAttemptAt.UTC()
	t.attempts++
	t.nextAttemptAt = &at
	t.updatedAt = now.UTC()
	return nil
}

// ResolveReference registra a transação referenciada, já encontrada.
func (t *Transaction) ResolveReference(id TransactionID) error {
	if t == nil || t.status == "" {
		return ErrUninitialized
	}
	if !t.kind.IsReversal() {
		return fmt.Errorf("%w: %s não tem referência", ErrUnexpectedReference, t.kind)
	}
	if id.IsZero() {
		return fmt.Errorf("%w: referência nula", ErrInvalidID)
	}
	t.referenceID = &id
	return nil
}

// Acessores.

func (t *Transaction) ID() TransactionID               { return t.id }
func (t *Transaction) Kind() Kind                      { return t.kind }
func (t *Transaction) Status() Status                  { return t.status }
func (t *Transaction) WalletID() wallet.ID             { return t.walletID }
func (t *Transaction) PlayerID() wallet.PlayerID       { return t.playerID }
func (t *Transaction) Amount() money.Money             { return t.amount }
func (t *Transaction) FailureCode() FailureCode        { return t.failureCode }
func (t *Transaction) Attempts() int                   { return t.attempts }
func (t *Transaction) CreatedAt() time.Time            { return t.createdAt }
func (t *Transaction) UpdatedAt() time.Time            { return t.updatedAt }
func (t *Transaction) IsExternal() bool                { return t.origin != nil }
func (t *Transaction) ReferenceExternalID() ExternalID { return t.referenceExternalID }

// Origin devolve os metadados externos, ou nil para operação interna.
func (t *Transaction) Origin() *ExternalOrigin {
	if t.origin == nil {
		return nil
	}
	copia := *t.origin
	return &copia
}

// ResultBalance devolve o saldo observado no processamento, se houver.
func (t *Transaction) ResultBalance() (money.Money, bool) {
	if t.resultBalance == nil {
		return money.Money{}, false
	}
	return *t.resultBalance, true
}

// ReferenceID devolve a transação referenciada, se já resolvida.
func (t *Transaction) ReferenceID() (TransactionID, bool) {
	if t.referenceID == nil {
		return TransactionID{}, false
	}
	return *t.referenceID, true
}

// NextAttemptAt devolve o instante da próxima tentativa, se agendada.
func (t *Transaction) NextAttemptAt() (time.Time, bool) {
	if t.nextAttemptAt == nil {
		return time.Time{}, false
	}
	return *t.nextAttemptAt, true
}

// ProcessedAt devolve o instante da conclusão, se houve.
func (t *Transaction) ProcessedAt() (time.Time, bool) {
	if t.processedAt == nil {
		return time.Time{}, false
	}
	return *t.processedAt, true
}
