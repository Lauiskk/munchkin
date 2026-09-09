package postgres

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Lauiskk/munchkin/internal/app/inbox"
)

// inboxRow é a linha da tabela de mensagens recebidas.
type inboxRow struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	ConsumerName string     `gorm:"column:consumer_name"`
	MessageID    string     `gorm:"column:message_id"`
	PayloadHash  []byte     `gorm:"column:payload_hash"`
	ReceivedAt   time.Time  `gorm:"column:received_at"`
	CompletedAt  *time.Time `gorm:"column:completed_at"`
}

func (inboxRow) TableName() string { return "inbox_messages" }

// InboxRepository registra as mensagens recebidas.
type InboxRepository struct{ db *Database }

// NewInboxRepository monta o repositório.
func NewInboxRepository(db *Database) *InboxRepository { return &InboxRepository{db: db} }

// Claim registra a mensagem, ou devolve o que já havia.
//
// Exige transação aberta. O registro e o efeito de domínio precisam ser
// indivisíveis: registrar fora da transação faria uma mensagem constar como
// recebida sem que o dinheiro tivesse se movido, e a reentrega — que é a única
// chance de corrigir — seria descartada como duplicata.
func (r *InboxRepository) Claim(
	ctx context.Context, consumidor, messageID string, hash []byte,
) (inbox.ClaimResult, error) {
	if !InTransaction(ctx) {
		return inbox.ClaimResult{}, fmt.Errorf("%w: Claim de mensagem", ErrNoTransaction)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return inbox.ClaimResult{}, fmt.Errorf("identificador da mensagem: %w", err)
	}

	// A inserção É a verificação. Consultar antes abriria janela entre o
	// SELECT e o INSERT, e dois consumidores tratariam a mesma mensagem.
	//
	// O desfecho vem de RowsAffected, e não de RETURNING: o driver entrega o
	// identificador como texto, e ler texto dentro de um uuid.UUID — que é um
	// arranjo de bytes — falha na conversão. Contar linhas responde a mesma
	// pergunta sem depender de conversão nenhuma.
	res := r.db.Session(ctx).Exec(`
		INSERT INTO inbox_messages (id, consumer_name, message_id, payload_hash)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		id, consumidor, messageID, hash)
	if res.Error != nil {
		return inbox.ClaimResult{}, classify(res.Error)
	}
	if res.RowsAffected == 1 {
		return inbox.ClaimResult{SameHash: true}, nil
	}

	// Já existia. O FOR UPDATE serializa dois tratamentos concorrentes da mesma
	// mensagem: sem ele, os dois leriam "não concluída" e processariam juntos.
	var linha inboxRow
	err = r.db.Session(ctx).Raw(`
		SELECT * FROM inbox_messages
		 WHERE consumer_name = ? AND message_id = ?
		   FOR UPDATE`, consumidor, messageID).Scan(&linha).Error
	if err != nil {
		return inbox.ClaimResult{}, classify(err)
	}
	if linha.ID == uuid.Nil {
		return inbox.ClaimResult{}, fmt.Errorf(
			"mensagem %s/%s sumiu entre o conflito e a leitura", consumidor, messageID)
	}

	return inbox.ClaimResult{
		Completed: linha.CompletedAt != nil,
		SameHash:  bytes.Equal(linha.PayloadHash, hash),
	}, nil
}

// Complete marca o tratamento como durável.
//
// A condição exige que ainda não esteja concluída. Se não afetar linha alguma,
// algo concluiu a mensagem por outro caminho enquanto esta transação corria, e
// isso precisa falhar alto em vez de sobrescrever em silêncio.
func (r *InboxRepository) Complete(ctx context.Context, consumidor, messageID string) error {
	if !InTransaction(ctx) {
		return fmt.Errorf("%w: Complete de mensagem", ErrNoTransaction)
	}

	res := r.db.Session(ctx).Exec(`
		UPDATE inbox_messages
		   SET completed_at = now()
		 WHERE consumer_name = ? AND message_id = ? AND completed_at IS NULL`,
		consumidor, messageID)
	if res.Error != nil {
		return classify(res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("conclusão da mensagem %s/%s afetou %d linhas",
			consumidor, messageID, res.RowsAffected)
	}
	return nil
}
