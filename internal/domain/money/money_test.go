package money_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/money"
)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, money.BRL)
	require.NoError(t, err, "valor de apoio do teste precisa ser válido: %q", amount)
	return m
}

// AC-1, AC-2, AC-3: o parsing produz a representação interna exata.
func TestParsePreservaOValorExatamente(t *testing.T) {
	casos := []struct {
		entrada string
		minor   int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.99", 99},
		{"1.00", 100},
		{"25.00", 2500},
		{"25.37", 2537},
		{"1000.00", 100000},
		{"-25.00", -2500},
		{"-0.01", -1},
		// Os valores que fazem ponto flutuante errar. 0.1 + 0.2 em float64 dá
		// 0.30000000000000004; em unidades mínimas, 10 + 20 = 30.
		{"0.10", 10},
		{"0.20", 20},
		{"0.30", 30},
		// Limite superior da faixa representável.
		{"92233720368547758.07", math.MaxInt64},
	}
	for _, c := range casos {
		t.Run(c.entrada, func(t *testing.T) {
			m, err := money.Parse(c.entrada, money.BRL)
			require.NoError(t, err)
			assert.Equal(t, c.minor, m.Minor())
			assert.Equal(t, money.BRL, m.Currency())
			assert.Equal(t, c.entrada, m.Amount(), "ida e volta sem perda")
		})
	}
}

// A soma que ponto flutuante erra. É o teste que justifica o tipo existir.
func TestSomaQueFlutuanteErraria(t *testing.T) {
	a, b := brl(t, "0.10"), brl(t, "0.20")

	soma, err := a.Add(b)
	require.NoError(t, err)

	assert.Equal(t, "0.30", soma.Amount())
	assert.True(t, soma.Equal(brl(t, "0.30")))
}

// AC-E1: entradas malformadas são recusadas, sem tentativa de adivinhar
// intenção e sem arredondamento silencioso.
func TestParseRecusaEntradaMalformada(t *testing.T) {
	casos := map[string]string{
		"vazio":                   "",
		"espaço":                  " ",
		"sem decimais":            "25",
		"ponto sem decimais":      "25.",
		"sem parte inteira":       ".00",
		"uma casa decimal":        "25.0",
		"três casas decimais":     "25.000",
		"escala excedente":        "25.999",
		"texto":                   "abc",
		"NaN":                     "NaN",
		"Infinity":                "Infinity",
		"infinito negativo":       "-Inf",
		"notação científica":      "1e2",
		"científica maiúscula":    "1E2",
		"hexadecimal":             "0x19",
		"vírgula decimal":         "25,00",
		"sinal positivo":          "+25.00",
		"espaço à esquerda":       " 25.00",
		"espaço à direita":        "25.00 ",
		"separador de milhar":     "1,000.00",
		"underscore":              "1_000.00",
		"dois pontos":             "25.00.00",
		"dois sinais":             "--25.00",
		"sinal sem valor":         "-",
		"zeros à esquerda":        "025.00",
		"dígito de largura dupla": "２５.００",
		"algarismo árabe":         "٢٥.٠٠",
	}
	for nome, entrada := range casos {
		t.Run(nome, func(t *testing.T) {
			_, err := money.Parse(entrada, money.BRL)
			require.Error(t, err, "%q deveria ser recusado", entrada)
		})
	}
}

// A mensagem de recusa precisa dizer o que está errado. Um provedor integrando
// recebe este texto, e "'.' não é um algarismo" para "25.00.00" manda ele
// procurar no lugar errado. As verificações que existem só para produzir uma
// mensagem melhor precisam de teste que as alcance, senão viram código que
// alguém remove por parecer redundante.
func TestMensagemDeRecusaIdentificaOProblema(t *testing.T) {
	casos := map[string]string{
		"25.00.00": "mais de um separador",
		"25":       "casas decimais",
		"25.0":     "casas decimais",
		"25.000":   "casas decimais",
		".00":      "parte inteira",
		"025.00":   "zeros à esquerda",
		"-0.00":    "zero negativo",
		"":         "vazio",
		"-":        "sinal sem valor",
	}
	for entrada, trecho := range casos {
		t.Run(entrada, func(t *testing.T) {
			_, err := money.Parse(entrada, money.BRL)
			require.Error(t, err)
			assert.Contains(t, err.Error(), trecho,
				"a mensagem para %q precisa apontar a causa", entrada)
		})
	}
}

// AC-E2: valor acima do representável é recusado no parsing, sem dar a volta.
func TestParseRecusaValorAcimaDoRepresentavel(t *testing.T) {
	casos := []string{
		"92233720368547758.08", // um centavo acima do máximo
		"99999999999999999.99",
		"-92233720368547758.09",
	}
	for _, entrada := range casos {
		t.Run(entrada, func(t *testing.T) {
			_, err := money.Parse(entrada, money.BRL)
			require.Error(t, err)
			assert.ErrorIs(t, err, money.ErrOverflow)
		})
	}
}

// A mensagem de erro não pode ecoar uma entrada arbitrariamente longa: um
// provedor mandaria megabytes e inflaria o log.
func TestErroNaoEcoaEntradaLonga(t *testing.T) {
	enorme := strings.Repeat("9", 10_000)

	_, err := money.Parse(enorme, money.BRL)
	require.Error(t, err)
	assert.Less(t, len(err.Error()), 200, "a mensagem não pode crescer com a entrada")
}

// AC-E3: moeda inválida é recusada.
func TestMoedaInvalidaEhRecusada(t *testing.T) {
	for _, code := range []string{"", "br", "brl", "BRLL", "BR1", "XYZ", "BR", "  BRL"} {
		t.Run(code, func(t *testing.T) {
			_, err := money.ParseCurrency(code)
			require.Error(t, err)
			assert.ErrorIs(t, err, money.ErrInvalidCurrency)
		})
	}
}

func TestMoedaConhecidaEhAceita(t *testing.T) {
	c, err := money.ParseCurrency("BRL")
	require.NoError(t, err)
	assert.Equal(t, money.BRL, c)
	assert.Equal(t, "BRL", c.String())
	assert.False(t, c.IsZero())
}

// AC-E4: valor não inicializado é recusado por qualquer operação. O enunciado
// exige que valor de domínio não inicializado seja rejeitado.
func TestValorNaoInicializadoEhRecusado(t *testing.T) {
	var vazio money.Money
	valido := brl(t, "10.00")

	assert.False(t, vazio.IsValid())
	assert.False(t, vazio.IsZero(), "não inicializado não é zero: é ausência de valor")

	_, err := vazio.Add(valido)
	assert.ErrorIs(t, err, money.ErrUninitialized)

	_, err = valido.Add(vazio)
	assert.ErrorIs(t, err, money.ErrUninitialized)

	_, err = vazio.Neg()
	assert.ErrorIs(t, err, money.ErrUninitialized)

	_, err = vazio.Cmp(valido)
	assert.ErrorIs(t, err, money.ErrUninitialized)

	var outroVazio money.Money
	assert.False(t, vazio.Equal(outroVazio),
		"dois valores ausentes não são iguais: ausência não é igualdade")
	assert.Empty(t, vazio.Amount())
}

// AC-5: moedas diferentes nunca se misturam, e a conversão implícita não existe.
func TestOperacaoEntreMoedasDiferentesEhErro(t *testing.T) {
	reais := brl(t, "10.00")
	dolares, err := money.Parse("10.00", money.USD)
	require.NoError(t, err)

	_, err = reais.Add(dolares)
	assert.ErrorIs(t, err, money.ErrCurrencyMismatch)

	_, err = reais.Sub(dolares)
	assert.ErrorIs(t, err, money.ErrCurrencyMismatch)

	_, err = reais.Cmp(dolares)
	assert.ErrorIs(t, err, money.ErrCurrencyMismatch)

	// Equal não é erro: valores de moedas diferentes simplesmente não são
	// iguais, e essa é uma resposta legítima.
	assert.False(t, reais.Equal(dolares))
}

// AC-4
func TestAritmetica(t *testing.T) {
	t.Run("soma", func(t *testing.T) {
		r, err := brl(t, "25.50").Add(brl(t, "74.50"))
		require.NoError(t, err)
		assert.Equal(t, "100.00", r.Amount())
	})

	t.Run("subtração", func(t *testing.T) {
		r, err := brl(t, "100.00").Sub(brl(t, "80.00"))
		require.NoError(t, err)
		assert.Equal(t, "20.00", r.Amount())
	})

	// AC-E5: negativo interno é permitido. Quem proíbe saldo negativo é a
	// carteira, não o tipo — diferenças precisam de sinal.
	t.Run("subtração pode resultar negativo", func(t *testing.T) {
		r, err := brl(t, "20.00").Sub(brl(t, "80.00"))
		require.NoError(t, err)
		assert.Equal(t, "-60.00", r.Amount())
		assert.True(t, r.IsNegative())
	})

	t.Run("negação", func(t *testing.T) {
		r, err := brl(t, "25.00").Neg()
		require.NoError(t, err)
		assert.Equal(t, "-25.00", r.Amount())

		devolta, err := r.Neg()
		require.NoError(t, err)
		assert.True(t, devolta.Equal(brl(t, "25.00")))
	})

	t.Run("operações não mutam o receptor", func(t *testing.T) {
		original := brl(t, "50.00")
		_, err := original.Add(brl(t, "10.00"))
		require.NoError(t, err)
		_, err = original.Neg()
		require.NoError(t, err)

		assert.Equal(t, "50.00", original.Amount(), "Money é imutável")
	})
}

// AC-6, AC-7: o estouro é conferido ANTES da operação. Em Go o estouro de
// inteiro com sinal não gera pânico: ele dá a volta em silêncio, e num sistema
// financeiro isso vira um saldo absurdo sem nenhum erro.
func TestEstouroEhDetectadoAntesDeAcontecer(t *testing.T) {
	maximo, err := money.New(math.MaxInt64, money.BRL)
	require.NoError(t, err)
	minimo, err := money.New(math.MinInt64+1, money.BRL)
	require.NoError(t, err)

	// O somando é DOIS centavos, não um, e a diferença importa: MaxInt64 + 1
	// dá exatamente MinInt64, que o construtor já recusa por outro motivo — o
	// teste passaria mesmo sem a verificação de estouro na soma. Com dois, o
	// resultado da volta é um valor que o construtor aceitaria, então só a
	// verificação em Add impede que ele apareça. Descoberto por teste de
	// mutação, não por leitura.
	t.Run("soma acima do máximo", func(t *testing.T) {
		doisCentavos := brl(t, "0.02")
		_, err := maximo.Add(doisCentavos)
		assert.ErrorIs(t, err, money.ErrOverflow)
	})

	// Mesmo cuidado da soma: subtrair UM centavo do mínimo daria exatamente
	// MinInt64, recusado pelo construtor por outro motivo. Com dois, o
	// resultado da volta é aceitável e só a verificação em Sub o impede.
	t.Run("subtração abaixo do mínimo", func(t *testing.T) {
		doisCentavos := brl(t, "0.02")
		_, err := minimo.Sub(doisCentavos)
		assert.ErrorIs(t, err, money.ErrOverflow)
	})

	t.Run("soma de negativo abaixo do mínimo", func(t *testing.T) {
		negativo, err := brl(t, "0.02").Neg()
		require.NoError(t, err)
		_, err = minimo.Add(negativo)
		assert.ErrorIs(t, err, money.ErrOverflow)
	})

	// A faixa é simétrica por decisão: o menor int64 é recusado na construção,
	// porque sua negação não seria representável e ele quebraria Neg depois.
	t.Run("o menor int64 não pode existir como Money", func(t *testing.T) {
		_, err := money.New(math.MinInt64, money.BRL)
		require.Error(t, err)
		assert.ErrorIs(t, err, money.ErrOverflow)
	})
}

// AC-8, AC-9, AC-10
func TestSerializacao(t *testing.T) {
	t.Run("o valor sai como string, nunca como número JSON", func(t *testing.T) {
		corpo, err := json.Marshal(brl(t, "25.00"))
		require.NoError(t, err)
		assert.JSONEq(t, `{"amount":"25.00","currency":"BRL"}`, string(corpo))

		// Número JSON viraria float64 na maioria dos leitores, e o valor
		// perderia precisão antes de o código Go sequer vê-lo.
		assert.Contains(t, string(corpo), `"25.00"`)
	})

	t.Run("ida e volta preserva o valor", func(t *testing.T) {
		for _, entrada := range []string{"0.00", "0.01", "25.37", "-60.00", "92233720368547758.07"} {
			original := brl(t, entrada)

			corpo, err := json.Marshal(original)
			require.NoError(t, err)

			var lido money.Money
			require.NoError(t, json.Unmarshal(corpo, &lido))
			assert.True(t, original.Equal(lido), "%s não sobreviveu à ida e volta", entrada)
		}
	})

	t.Run("desserialização recusa corpo inválido", func(t *testing.T) {
		casos := []string{
			`{"amount":25.00,"currency":"BRL"}`, // número em vez de string
			`{"amount":"25.000","currency":"BRL"}`,
			`{"amount":"25.00","currency":"XYZ"}`,
			`{"amount":"25.00"}`,
			`{"currency":"BRL"}`,
			`{"amount":"","currency":"BRL"}`,
			`"25.00 BRL"`,
		}
		for _, corpo := range casos {
			var m money.Money
			assert.Error(t, json.Unmarshal([]byte(corpo), &m), "%s deveria ser recusado", corpo)
		}
	})

	t.Run("sempre duas casas decimais", func(t *testing.T) {
		for entrada, esperado := range map[string]string{
			"0.00": "0.00", "1.00": "1.00", "0.05": "0.05", "10.50": "10.50",
		} {
			assert.Equal(t, esperado, brl(t, entrada).Amount())
		}
	})

	t.Run("String carrega a moeda", func(t *testing.T) {
		assert.Equal(t, "25.00 BRL", brl(t, "25.00").String())

		var vazio money.Money
		assert.Equal(t, "<money inválido>", vazio.String())
	})
}

func TestComparacao(t *testing.T) {
	dez, vinte := brl(t, "10.00"), brl(t, "20.00")

	menor, err := dez.Cmp(vinte)
	require.NoError(t, err)
	assert.Equal(t, -1, menor)

	maior, err := vinte.Cmp(dez)
	require.NoError(t, err)
	assert.Equal(t, 1, maior)

	igual, err := dez.Cmp(brl(t, "10.00"))
	require.NoError(t, err)
	assert.Zero(t, igual)

	zero, err := money.Zero(money.BRL)
	require.NoError(t, err)
	assert.True(t, zero.IsZero())
	assert.False(t, zero.IsPositive())
	assert.False(t, zero.IsNegative())
	assert.True(t, dez.IsPositive())
}

func TestZeroDaMesmaMoeda(t *testing.T) {
	m := brl(t, "25.00")
	z := m.ZeroOfSame()

	assert.True(t, z.IsZero())
	assert.Equal(t, money.BRL, z.Currency())
}

// O fuzzing procura entrada que o parser aceite e que não sobreviva à ida e
// volta — a classe de defeito que uma lista de casos escrita à mão não alcança,
// porque ela só contém o que alguém pensou em escrever.
func FuzzParseIdaEVolta(f *testing.F) {
	for _, s := range []string{
		"0.00", "25.00", "-25.37", "92233720368547758.07",
		"25", "25.000", "abc", "", "1e2", "025.00", "-",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, entrada string) {
		m, err := money.Parse(entrada, money.BRL)
		if err != nil {
			return // recusar é resposta legítima
		}

		// Aceito precisa significar reproduzível: o texto formatado tem de
		// voltar ao mesmo valor interno.
		formatado := m.Amount()
		reparsed, err := money.Parse(formatado, money.BRL)
		if err != nil {
			t.Fatalf("Parse aceitou %q, formatou como %q, e recusou o próprio formato: %v",
				entrada, formatado, err)
		}
		if reparsed.Minor() != m.Minor() {
			t.Fatalf("ida e volta perdeu valor: %q -> %d -> %q -> %d",
				entrada, m.Minor(), formatado, reparsed.Minor())
		}

		// E o formato canônico tem de ser o próprio texto aceito: uma entrada
		// aceita que formate diferente seria uma grafia alternativa, e grafias
		// alternativas produzem hashes de idempotência diferentes para o mesmo
		// negócio.
		if formatado != entrada {
			t.Fatalf("grafia alternativa aceita: %q formata como %q", entrada, formatado)
		}
	})
}
