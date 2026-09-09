package dto_test

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/http/dto"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
)

func TestCursorSobreviveAoTrajetoDeIdaEVolta(t *testing.T) {
	original := appwallet.Position{
		// Precisão de micro: é o que o PostgreSQL guarda num timestamptz, e
		// perder o resto do instante faria o desempate da página seguinte cair
		// no lugar errado.
		CreatedAt: time.Date(2026, 9, 9, 12, 0, 0, 123456000, time.UTC),
		ID:        uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d"),
	}

	lido, err := dto.DecodeCursor(dto.EncodeCursor(original))

	require.NoError(t, err)
	assert.True(t, original.CreatedAt.Equal(lido.CreatedAt),
		"esperado %v, veio %v", original.CreatedAt, lido.CreatedAt)
	assert.Equal(t, original.ID, lido.ID)
}

func TestCursorNaoVazaFormatoNaURL(t *testing.T) {
	c := dto.EncodeCursor(appwallet.Position{CreatedAt: time.Now(), ID: uuid.New()})

	assert.NotContains(t, c, "|")
	assert.NotContains(t, c, "+", "base64 padrão quebraria em query string")
	assert.NotContains(t, c, "/")
	assert.NotContains(t, c, "=", "o padding também não sobrevive a uma URL")
}

// Falha fechado. Um cursor corrompido tratado como "começar do início"
// devolveria a página errada em silêncio, e quem pagina não teria como
// perceber — num extrato financeiro, pular uma linha é o defeito.
func TestCursorInvalidoEhRecusado(t *testing.T) {
	valido := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	cru := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

	casos := map[string]string{
		"vazio":                  "",
		"não é base64":           "não!!!base64",
		"base64 sem separador":   cru("apenas-uma-parte"),
		"partes demais":          cru(valido + "|" + uuid.NewString() + "|extra"),
		"instante inválido":      cru("ontem|" + uuid.NewString()),
		"identificador inválido": cru(valido + "|não-é-uuid"),
		"invertido":              cru(uuid.NewString() + "|" + valido),
	}

	for nome, entrada := range casos {
		t.Run(nome, func(t *testing.T) {
			_, err := dto.DecodeCursor(entrada)
			assert.ErrorIs(t, err, dto.ErrCursorInvalido)
		})
	}
}
