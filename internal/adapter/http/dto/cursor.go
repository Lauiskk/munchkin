package dto

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
)

// ErrCursorInvalido indica cursor corrompido, truncado ou de outro formato.
var ErrCursorInvalido = errors.New("cursor inválido")

// separadorCursor separa as duas partes da posição. Não aparece nem em RFC 3339
// nem em UUID, então não há ambiguidade ao dividir.
const separadorCursor = "|"

// EncodeCursor transforma uma posição em texto opaco.
//
// Opaco é contrato, não segredo: o cliente não deve interpretar nem construir,
// para que a chave de paginação possa mudar sem quebrar quem integra. Por isso
// não é assinado — ele apenas posiciona dentro da carteira do path, que o
// chamador já está autorizado a ler, e forjá-lo não dá acesso a nada novo.
func EncodeCursor(p appwallet.Position) string {
	bruto := p.CreatedAt.UTC().Format(time.RFC3339Nano) + separadorCursor + p.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(bruto))
}

// DecodeCursor lê a posição de volta.
//
// Falha fechado: qualquer coisa fora do formato vira erro, e nunca uma consulta
// sem filtro. Um cursor corrompido que fosse tratado como "começar do início"
// devolveria silenciosamente a página errada, e quem pagina não teria como
// perceber.
func DecodeCursor(s string) (appwallet.Position, error) {
	bruto, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return appwallet.Position{}, ErrCursorInvalido
	}

	partes := strings.Split(string(bruto), separadorCursor)
	if len(partes) != 2 {
		return appwallet.Position{}, ErrCursorInvalido
	}

	instante, err := time.Parse(time.RFC3339Nano, partes[0])
	if err != nil {
		return appwallet.Position{}, ErrCursorInvalido
	}
	id, err := uuid.Parse(partes[1])
	if err != nil {
		return appwallet.Position{}, ErrCursorInvalido
	}

	return appwallet.Position{CreatedAt: instante, ID: id}, nil
}
