package wagering

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// Este truncar recebe entrada EXTERNA — o `kind`, o `status` e o código de falha
// que chegaram no corpo da requisição — e o resultado vai para a mensagem de
// erro devolvida ao cliente. Quem manda escolhe os bytes.
func TestTruncarNuncaProduzUTF8Invalido(t *testing.T) {
	for _, caractere := range []string{"ê", "ç", "ã", "€", "日", "🙂"} {
		for n := 1; n <= 80; n++ {
			cortado := truncar(strings.Repeat(caractere, n))
			require.True(t, utf8.ValidString(cortado),
				"corte de %d %q produziu bytes inválidos: %v", n, caractere, []byte(cortado))
		}
	}
}
