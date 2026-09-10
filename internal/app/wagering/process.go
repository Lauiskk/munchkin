package wagering

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Lauiskk/munchkin/internal/app"
	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/correlation"
)

// primeiroBackoff é a espera até a primeira retomada de uma referência
// pendente. As seguintes crescem exponencialmente, com teto — ver o worker.
const primeiroBackoff = 5 * time.Second

var (
	// ErrIdempotencyConflict indica reuso de chave com conteúdo diferente, ou
	// a mesma operação enviada com outra chave. Nada é gravado.
	ErrIdempotencyConflict = errors.New("conflito de idempotência")
	// ErrProviderMismatch indica corpo divergindo do provedor do token.
	ErrProviderMismatch = errors.New("o provedor do corpo difere do token")
)

// Input é a operação recebida, já validada na fronteira.
type Input struct {
	// ProviderID vem do TOKEN, nunca do corpo. É o que sustenta o isolamento
	// entre provedores.
	ProviderID     domain.ProviderID
	ExternalID     domain.ExternalID
	IdempotencyKey string

	PlayerID wallet.PlayerID
	WalletID wallet.ID
	RoundID  domain.RoundID
	GameID   domain.GameID
	Kind     domain.Kind
	Money    money.Money

	ReferenceExternalID domain.ExternalID

	// Source é por onde a operação entrou. Não participa do hash de
	// idempotência — a mesma operação por HTTP e por fila é a MESMA operação —
	// e existe só para que a métrica saiba distinguir os caminhos.
	Source string
}

// Output é o desfecho, do ponto de vista do provedor.
//
// Recusa de negócio NÃO é erro aqui: ela é um desfecho legítimo, com registro
// persistido e evento emitido. Erro fica reservado para conflito de idempotência
// e falha de infraestrutura.
type Output struct {
	TransactionID    domain.TransactionID
	Status           domain.Status
	Balance          money.Money
	FailureCode      domain.FailureCode
	IdempotentReplay bool
}

// Processor executa operações financeiras.
type Processor struct {
	tx           TxManager
	wallets      WalletRepository
	transactions TransactionRepository
	entries      LedgerRepository
	outbox       OutboxRepository
	clock        app.Clock
	metrics      app.Metrics
}

// NewProcessor monta o caso de uso.
func NewProcessor(
	tx TxManager, wallets WalletRepository, transactions TransactionRepository,
	entries LedgerRepository, outbox OutboxRepository, clock app.Clock,
	metrics app.Metrics,
) *Processor {
	return &Processor{
		tx: tx, wallets: wallets, transactions: transactions,
		entries: entries, outbox: outbox, clock: clock, metrics: metrics,
	}
}

// Process aplica a operação.
//
// Todo o trabalho acontece numa transação SQL, nesta ordem, que é fixa e
// importa:
//
//  1. trava a linha da carteira — serializa os escritores DAQUELA carteira;
//  2. valida o alvo (existe, moeda, jogador);
//  3. reivindica a idempotência inserindo a operação;
//  4. o domínio decide;
//  5. grava saldo, lançamento, desfecho e eventos;
//  6. commita — e só aqui os eventos passam a existir.
//
// A carteira é travada ANTES da reivindicação porque a operação referencia a
// carteira por chave estrangeira: uma operação para carteira inexistente não
// pode ser inserida, então precisa ser recusada antes da tentativa. O custo é
// que um replay também toma o lock; a alternativa seria uma consulta extra no
// caminho de toda operação nova, que é o caminho quente.
func (p *Processor) Process(ctx context.Context, in Input) (Output, error) {
	fingerprint := domain.Fingerprint{
		ProviderID:          in.ProviderID,
		ExternalID:          in.ExternalID,
		PlayerID:            in.PlayerID,
		WalletID:            in.WalletID,
		RoundID:             in.RoundID,
		GameID:              in.GameID,
		Kind:                in.Kind,
		Amount:              in.Money,
		ReferenceExternalID: in.ReferenceExternalID,
	}
	hash, err := fingerprint.Hash()
	if err != nil {
		return Output{}, err
	}

	inicio := p.clock.Now()

	var out Output
	err = p.tx.Within(ctx, func(ctx context.Context) error {
		var erroInterno error
		out, erroInterno = p.executar(ctx, in, hash)
		return erroInterno
	})
	if err != nil {
		return out, err
	}

	// A medição fica DEPOIS do commit, e só no caminho bem-sucedido: medir
	// dentro da transação incluiria o tempo do próprio registro, e medir o
	// caminho que falhou misturaria latência de trabalho com latência de erro.
	p.registrar(in, out, p.clock.Now().Sub(inicio))
	return out, nil
}

// registrar publica o que aconteceu. Nunca altera o resultado.
func (p *Processor) registrar(in Input, out Output, duracao time.Duration) {
	origem := in.Source
	if origem == "" {
		origem = app.SourceHTTP
	}

	if out.IdempotentReplay {
		p.metrics.IdempotentReplay(origem)
		return
	}

	p.metrics.TransactionSettled(
		string(in.Kind), string(out.Status), string(out.FailureCode), origem)
	p.metrics.ProcessingDuration(string(in.Kind), origem, duracao)
}

func (p *Processor) executar(ctx context.Context, in Input, hash []byte) (Output, error) {
	agora := p.clock.Now()

	// 1. Trava a carteira. Daqui até o commit, nenhuma outra transação mexe
	//    nesta carteira — e carteiras distintas seguem em paralelo.
	w, err := p.wallets.LockByID(ctx, in.WalletID)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return recusaSemRegistro(domain.FailureWalletNotFound), nil
		}
		return Output{}, err
	}

	// 2. Valida o alvo. Estas recusas não são gravadas: uma operação que não
	//    endereça uma carteira válida não é dado de auditoria, é entrada
	//    errada — e, no caso da moeda, o schema literalmente não a comporta,
	//    porque a chave estrangeira é composta por (carteira, moeda).
	if !w.BelongsTo(in.PlayerID) {
		return recusaSemRegistro(domain.FailureWalletPlayerMismatch), nil
	}
	if in.Money.Currency() != w.Currency() {
		return recusaSemRegistro(domain.FailureCurrencyMismatch), nil
	}

	// 3. Reivindica a idempotência. O INSERT é a verificação.
	txID, err := domain.NewTransactionID()
	if err != nil {
		return Output{}, err
	}
	operacao, err := domain.NewExternal(txID, in.Kind, w.ID(), w.PlayerID(), in.Money,
		domain.ExternalOrigin{
			ProviderID:     in.ProviderID,
			ExternalID:     in.ExternalID,
			IdempotencyKey: in.IdempotencyKey,
			PayloadHash:    hash,
			RoundID:        in.RoundID,
			GameID:         in.GameID,
		}, in.ReferenceExternalID, agora)
	if err != nil {
		// Valor incompatível com o tipo é recusa de negócio, não erro.
		if errors.Is(err, domain.ErrInvalidAmountForKind) {
			return recusaSemRegistro(domain.FailureInvalidAmountForKind), nil
		}
		return Output{}, err
	}

	claim, err := p.transactions.Claim(ctx, operacao)
	if err != nil {
		return Output{}, err
	}
	if !claim.Claimed {
		return p.decidirReplay(claim.Existing, in, hash)
	}

	// 4 e 5. O domínio decide e o resultado é gravado.
	return p.aplicar(ctx, w, operacao, agora)
}

// aplicar decide e grava.
func (p *Processor) aplicar(
	ctx context.Context, w *wallet.Wallet, operacao *domain.Transaction, agora time.Time,
) (Output, error) {
	// A versão é capturada ANTES de decidir, porque decidir MUTA a carteira: um
	// débito ou crédito já incrementa a versão do agregado. Capturá-la depois
	// faria o UPDATE condicionado procurar pela versão nova e não encontrar
	// linha nenhuma. Foi exatamente o defeito que a primeira versão desta
	// refatoração introduziu, e que só o banco revelou.
	versaoAnterior := w.Version()

	d, err := p.decidir(ctx, w, operacao, agora)
	// Um crédito que estoura o representável é RECUSA, não defeito: a entrada é
	// válida, a carteira existe, e o resultado é o mesmo toda vez que tentarem.
	// Sem este desvio o erro cai no caso genérico e vira 500 — que diz "há um
	// bug aqui, não repita" para algo determinístico e auditável. Cobre WIN,
	// REFUND e ROLLBACK de uma vez porque os três creditam.
	if errors.Is(err, money.ErrOverflow) {
		d, err = decisao{recusa: domain.FailureBalanceLimitExceeded}, nil
	}
	if err != nil {
		return Output{}, err
	}
	return p.aplicarDecisao(ctx, w, operacao, d, versaoAnterior, agora)
}

// aplicarDecisao grava o desfecho já decidido.
//
// É separado de aplicar para que o resolvedor de pendências reaproveite
// exatamente este caminho. Se a retomada gravasse por conta própria, ela e a
// entrada direta divergiriam com o tempo — e o sistema passaria a dar respostas
// diferentes para a mesma operação conforme o caminho por onde ela entrou.
func (p *Processor) aplicarDecisao(
	ctx context.Context, w *wallet.Wallet, operacao *domain.Transaction,
	d decisao, versaoAnterior int64, agora time.Time,
) (Output, error) {

	// A referência ainda não chegou. A operação é persistida como pendente e um
	// worker retoma depois — inclusive após reinício, porque o estado está no
	// banco e não na memória deste processo.
	if d.aguardar {
		if err := operacao.MarkPendingReference(agora.Add(primeiroBackoff), agora); err != nil {
			return Output{}, err
		}
		if err := p.transactions.Settle(ctx, operacao); err != nil {
			return Output{}, err
		}
		if err := p.publicarPendencia(ctx, operacao, agora); err != nil {
			return Output{}, err
		}
		return Output{
			TransactionID: operacao.ID(),
			Status:        domain.PendingReference,
			Balance:       w.Balance(),
		}, nil
	}

	movimento, recusa := d.movimento, d.recusa
	if recusa != "" {
		// Recusa de negócio COMMITA. O registro terminal e o evento precisam
		// sobreviver, senão o provedor reenviaria para sempre uma operação que
		// já tem desfecho. Como nada foi escrito no saldo, basta gravar o
		// desfecho — não é preciso savepoint.
		if err := operacao.MarkRejected(recusa, agora); err != nil {
			return Output{}, err
		}
		if err := p.transactions.Settle(ctx, operacao); err != nil {
			return Output{}, err
		}
		if err := p.publicarRecusa(ctx, operacao, agora); err != nil {
			return Output{}, err
		}
		return Output{
			TransactionID: operacao.ID(),
			Status:        domain.Rejected,
			Balance:       w.Balance(),
			FailureCode:   recusa,
		}, nil
	}

	if movimento != nil {
		if err := p.wallets.UpdateBalance(ctx, w, versaoAnterior); err != nil {
			// Chegar aqui significa que a versão mudou apesar do lock da linha.
			// Não deveria acontecer, e é justamente por isso que interessa
			// contar: um contador que sobe aqui é sintoma de que alguma escrita
			// escapou do caminho travado.
			p.metrics.ConcurrencyConflict("wallet_version")
			return Output{}, err
		}
		entryID, err := ledger.NewID()
		if err != nil {
			return Output{}, err
		}
		entrada, err := ledger.FromMovement(entryID, w.ID(), operacao.ID(), *movimento, agora)
		if err != nil {
			return Output{}, err
		}
		if err := p.entries.Append(ctx, entrada); err != nil {
			return Output{}, err
		}
	}

	if err := operacao.MarkProcessed(w.Balance(), agora); err != nil {
		return Output{}, err
	}
	if err := p.transactions.Settle(ctx, operacao); err != nil {
		return Output{}, err
	}
	if err := p.publicarConclusao(ctx, w, operacao, movimento, agora); err != nil {
		return Output{}, err
	}

	return Output{
		TransactionID: operacao.ID(),
		Status:        domain.Processed,
		Balance:       w.Balance(),
	}, nil
}

// decisao é o que o domínio resolveu para a operação.
type decisao struct {
	movimento *wallet.Movement
	recusa    domain.FailureCode
	// aguardar indica que a referência ainda não chegou.
	aguardar bool
}

// decidir pede ao domínio o desfecho correspondente ao tipo.
func (p *Processor) decidir(
	ctx context.Context, w *wallet.Wallet, operacao *domain.Transaction, agora time.Time,
) (decisao, error) {
	switch operacao.Kind() {
	case domain.Bet:
		mv, err := w.Debit(operacao.Amount(), agora)
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return decisao{recusa: domain.FailureInsufficientFunds}, nil
		}
		if err != nil {
			return decisao{}, err
		}
		return decisao{movimento: &mv}, nil

	case domain.Win:
		mv, err := w.Credit(operacao.Amount(), agora)
		if err != nil {
			return decisao{}, err
		}
		return decisao{movimento: &mv}, nil

	case domain.Loss:
		// LOSS conclui sem movimentar: o dinheiro já saiu na aposta. Sem
		// movimentação não há lançamento, e a versão da carteira não avança.
		return decisao{}, nil

	case domain.Refund, domain.Rollback:
		return p.reverter(ctx, w, operacao, agora)

	default:
		return decisao{}, fmt.Errorf("tipo %s não é tratado", operacao.Kind())
	}
}

// reverter resolve a referência e aplica o movimento contrário ao original.
func (p *Processor) reverter(
	ctx context.Context, w *wallet.Wallet, operacao *domain.Transaction, agora time.Time,
) (decisao, error) {
	origem := operacao.Origin()

	// Uma operação não reverte a si mesma. Sem esta guarda, a busca encontraria
	// a própria linha recém-inserida e a reversão se auto-referenciaria.
	if operacao.ReferenceExternalID() == origem.ExternalID {
		return decisao{recusa: domain.FailureReferenceMismatch}, nil
	}

	// A referência é buscada pelo provedor da OPERAÇÃO, que veio do token. Sem
	// isso, um provedor estornaria a aposta de outro informando o identificador
	// dela.
	ref, err := p.transactions.FindByProviderExternalID(
		ctx, origem.ProviderID, operacao.ReferenceExternalID())
	if errors.Is(err, app.ErrNotFound) {
		return decisao{aguardar: true}, nil
	}
	if err != nil {
		return decisao{}, err
	}

	if recusa := conferirReferencia(operacao, ref); recusa != "" {
		return decisao{recusa: recusa}, nil
	}

	switch ref.Status() {
	case domain.Processed:
		// Segue.
	case domain.Pending, domain.PendingReference:
		// A referência existe mas ainda não tem desfecho. Aguardar é o certo:
		// o desfecho dela ainda pode mudar, e recusar agora tornaria a ordem de
		// chegada das mensagens parte da regra de negócio.
		return decisao{aguardar: true}, nil
	default:
		// Recusada ou falha permanente: não há o que reverter.
		return decisao{recusa: domain.FailureReferenceNotProcessed}, nil
	}

	// No máximo uma reversão bem-sucedida por referência. Sem esta guarda, um
	// REFUND e um ROLLBACK da mesma aposta devolveriam o mesmo débito duas
	// vezes — e o banco recusaria a segunda gravação com uma violação de índice
	// único, que vira erro interno em vez de resposta compreensível.
	//
	// A consulta não corre risco de corrida porque a carteira já está travada
	// desde o início da transação, e a reversão concorrente disputaria a mesma
	// carteira: a referência só passa por conferirReferencia se pertencer a
	// ela. Sob READ COMMITTED, quem chega depois lê o commit de quem passou.
	switch _, err := p.transactions.FindProcessedReversalOf(ctx, ref.ID()); {
	case err == nil:
		return decisao{recusa: domain.FailureReferenceAlreadyReversed}, nil
	case !errors.Is(err, app.ErrNotFound):
		return decisao{}, err
	}

	if err := operacao.ResolveReference(ref.ID()); err != nil {
		return decisao{}, err
	}

	// O movimento é o contrário do original: reverter uma aposta credita,
	// reverter um ganho debita.
	if ref.Kind().CreditsWallet() {
		mv, err := w.Debit(operacao.Amount(), agora)
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			// Código PRÓPRIO, distinto do de aposta sem saldo. Para quem
			// audita, "o jogador não tinha saldo para apostar" e "o dinheiro já
			// saiu da carteira, não dá para estornar" são situações diferentes.
			return decisao{recusa: domain.FailureReversalInsufficientFunds}, nil
		}
		if err != nil {
			return decisao{}, err
		}
		return decisao{movimento: &mv}, nil
	}

	mv, err := w.Credit(operacao.Amount(), agora)
	if err != nil {
		return decisao{}, err
	}
	return decisao{movimento: &mv}, nil
}

// conferirReferencia valida que operação e referência descrevem o mesmo negócio.
func conferirReferencia(operacao, ref *domain.Transaction) domain.FailureCode {
	if !operacao.Kind().CanReverse(ref.Kind()) {
		return domain.FailureReferenceMismatch
	}

	origem, refOrigem := operacao.Origin(), ref.Origin()
	if refOrigem == nil || origem.ProviderID != refOrigem.ProviderID {
		return domain.FailureReferenceMismatch
	}
	if origem.RoundID != refOrigem.RoundID {
		return domain.FailureReferenceMismatch
	}
	if operacao.WalletID() != ref.WalletID() || operacao.PlayerID() != ref.PlayerID() {
		return domain.FailureReferenceMismatch
	}
	if operacao.Amount().Currency() != ref.Amount().Currency() {
		return domain.FailureCurrencyMismatch
	}

	// Reversão parcial está fora do escopo: valor menor seria devolução
	// incompleta, maior seria criar dinheiro.
	if !operacao.Amount().Equal(ref.Amount()) {
		return domain.FailureAmountMismatch
	}
	return ""
}

// decidirReplay resolve o que fazer quando a identidade já estava ocupada.
func (p *Processor) decidirReplay(existente *domain.Transaction, in Input, hash []byte) (Output, error) {
	origem := existente.Origin()
	if origem == nil {
		return Output{}, fmt.Errorf("conflito com operação interna %s", existente.ID())
	}

	// A operação já foi aplicada com OUTRA chave. Aceitar seria permitir que a
	// mesma movimentação financeira fosse reaplicada trocando a chave.
	if origem.IdempotencyKey != in.IdempotencyKey {
		return Output{}, fmt.Errorf(
			"%w: a operação %s já foi registrada com outra chave de idempotência",
			ErrIdempotencyConflict, in.ExternalID)
	}

	// Mesma chave, conteúdo diferente.
	if !bytes.Equal(origem.PayloadHash, hash) {
		return Output{}, fmt.Errorf(
			"%w: a chave %s já foi usada com conteúdo diferente",
			ErrIdempotencyConflict, in.IdempotencyKey)
	}

	// Replay legítimo: devolve o resultado persistido, sem reaplicar nada. O
	// saldo é o OBSERVADO NO PROCESSAMENTO ORIGINAL, mesmo que a carteira já
	// tenha se movimentado depois — é o que o contrato promete.
	saldo, temSaldo := existente.ResultBalance()
	if !temSaldo {
		saldo = in.Money.ZeroOfSame()
	}
	return Output{
		TransactionID:    existente.ID(),
		Status:           existente.Status(),
		Balance:          saldo,
		FailureCode:      existente.FailureCode(),
		IdempotentReplay: true,
	}, nil
}

// recusaSemRegistro monta o desfecho de uma recusa que não é persistida.
func recusaSemRegistro(code domain.FailureCode) Output {
	return Output{Status: domain.Rejected, FailureCode: code}
}

func (p *Processor) publicarConclusao(
	ctx context.Context, w *wallet.Wallet, operacao *domain.Transaction,
	movimento *wallet.Movement, agora time.Time,
) error {
	origem := operacao.Origin()
	corr := correlation.From(ctx)

	if err := p.enfileirar(ctx, event.WagerTransactionProcessed{
		TransactionID: operacao.ID(),
		WalletID:      w.ID(),
		PlayerID:      w.PlayerID(),
		Kind:          operacao.Kind(),
		Money:         operacao.Amount(),
		Balance:       w.Balance(),
		ProviderID:    origem.ProviderID,
		ExternalID:    origem.ExternalID,
		RoundID:       origem.RoundID,
		GameID:        origem.GameID,
		ProcessedAt:   event.Timestamp(agora),
	}, corr); err != nil {
		return err
	}

	// LOSS conclui sem alterar o saldo, então não produz este evento. Emiti-lo
	// mesmo assim faria um consumidor registrar uma mudança que não houve.
	if movimento == nil {
		return nil
	}

	return p.enfileirar(ctx, event.WalletBalanceChanged{
		WalletID:      w.ID(),
		TransactionID: operacao.ID(),
		Direction:     movimento.Direction,
		Money:         movimento.Amount,
		BalanceBefore: movimento.BalanceBefore,
		BalanceAfter:  movimento.BalanceAfter,
		WalletVersion: w.Version(),
		ChangedAt:     event.Timestamp(agora),
	}, corr)
}

func (p *Processor) publicarPendencia(ctx context.Context, operacao *domain.Transaction, agora time.Time) error {
	origem := operacao.Origin()
	return p.enfileirar(ctx, event.WagerTransactionPendingReference{
		TransactionID:       operacao.ID(),
		WalletID:            operacao.WalletID(),
		PlayerID:            operacao.PlayerID(),
		Kind:                operacao.Kind(),
		Money:               operacao.Amount(),
		ProviderID:          origem.ProviderID,
		ExternalID:          origem.ExternalID,
		ReferenceExternalID: operacao.ReferenceExternalID(),
		PendingSince:        event.Timestamp(agora),
	}, correlation.From(ctx))
}

func (p *Processor) publicarRecusa(ctx context.Context, operacao *domain.Transaction, agora time.Time) error {
	origem := operacao.Origin()
	return p.enfileirar(ctx, event.WagerTransactionRejected{
		TransactionID: operacao.ID(),
		WalletID:      operacao.WalletID(),
		PlayerID:      operacao.PlayerID(),
		Kind:          operacao.Kind(),
		Money:         operacao.Amount(),
		FailureCode:   operacao.FailureCode(),
		ProviderID:    origem.ProviderID,
		ExternalID:    origem.ExternalID,
		RejectedAt:    event.Timestamp(agora),
	}, correlation.From(ctx))
}

func (p *Processor) enfileirar(ctx context.Context, payload event.Payload, correlationID string) error {
	id, err := event.NewID()
	if err != nil {
		return err
	}
	env, err := event.New(id, payload, correlationID, "", p.clock.Now())
	if err != nil {
		return err
	}
	return p.outbox.Enqueue(ctx, env)
}
