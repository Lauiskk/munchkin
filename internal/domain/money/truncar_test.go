package money

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A entrada vem de fora, e quem manda escolhe os bytes. Cortar por byte parte um
// caractere multibyte ao meio, e o resultado é UTF-8 inválido — que o PostgreSQL
// recusa numa coluna TEXT e o SQS recusa num atributo de mensagem.
//
// O teste varre TODOS os comprimentos ao redor do limite, porque o defeito só
// aparece quando a fronteira cai exatamente no meio de um caractere.
func TestTruncarNuncaProduzUTF8Invalido(t *testing.T) {
	for _, caractere := range []string{"ê", "ç", "ã", "€", "日", "🙂"} {
		t.Run(caractere, func(t *testing.T) {
			for n := 1; n <= 80; n++ {
				entrada := strings.Repeat(caractere, n)
				cortado := truncar(entrada)

				require.True(t, utf8.ValidString(cortado),
					"corte de %d %q produziu bytes inválidos: %v",
					n, caractere, []byte(cortado))
				assert.True(t, strings.HasPrefix(entrada, strings.TrimSuffix(cortado, "…")),
					"o corte tem de ser um prefixo do original")
			}
		})
	}
}

func TestTruncarPreservaEntradaCurta(t *testing.T) {
	assert.Equal(t, "BRL", truncar("BRL"))
	assert.Equal(t, "", truncar(""))
}
