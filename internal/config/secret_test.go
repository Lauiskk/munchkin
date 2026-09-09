package config_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/config"
)

const senha = "senha-super-secreta-do-banco"

// A forma mais comum de vazar credencial não é ataque: é um %+v numa depuração
// ou num erro que sobe até o log. Este teste percorre todos os caminhos por
// onde um valor costuma escapar.
func TestSegredoNaoVazaEmNenhumaFormatacao(t *testing.T) {
	s := config.Secret(senha)

	// Os verbos vêm de uma lista em vez de chamadas literais: é o próprio
	// comportamento de cada verbo que está sob teste, e escrever
	// fmt.Sprintf("%s", s) literalmente faria o analisador estático sugerir
	// trocá-lo por s.String() — o que desfaria o teste.
	for _, verbo := range []string{"%v", "%s", "%q", "%+v", "%#v"} {
		saida := fmt.Sprintf(verbo, s)
		assert.NotContains(t, saida, senha, "o segredo vazou em %s", verbo)
		assert.Contains(t, saida, "redigido", "%s deveria redigir", verbo)
	}

	for nome, saida := range map[string]string{"String()": s.String(), "Sprint": fmt.Sprint(s)} {
		assert.NotContains(t, saida, senha, "o segredo vazou em %s", nome)
		assert.Contains(t, saida, "redigido", "%s deveria redigir", nome)
	}
}

// Struct aninhada é o caso realmente perigoso: ninguém imprime o campo, imprime
// a configuração inteira.
func TestSegredoNaoVazaDentroDeStruct(t *testing.T) {
	cfg := struct {
		Host  string
		Senha config.Secret
	}{Host: "postgres", Senha: config.Secret(senha)}

	for _, saida := range []string{
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
	} {
		assert.NotContains(t, saida, senha)
		assert.Contains(t, saida, "postgres", "o resto da struct precisa continuar legível")
	}
}

func TestSegredoNaoVazaEmJSON(t *testing.T) {
	corpo, err := json.Marshal(struct {
		Senha config.Secret `json:"senha"`
	}{config.Secret(senha)})

	require.NoError(t, err)
	assert.NotContains(t, string(corpo), senha)
	assert.JSONEq(t, `{"senha":"[redigido]"}`, string(corpo))
}

// Revelar é o único caminho para o valor real, e é deliberadamente explícito:
// um grep por Reveal( lista todos os lugares que tocam a credencial.
func TestRevealDevolveOValorReal(t *testing.T) {
	s := config.Secret(senha)

	assert.Equal(t, senha, s.Reveal())
	assert.False(t, s.IsEmpty())
	assert.True(t, config.Secret("").IsEmpty())
}

// A string de conexão precisa do valor real — é a exceção legítima, e o teste
// registra que ela é intencional.
func TestDSNCarregaASenhaRealPorNecessidade(t *testing.T) {
	db := config.DB{
		Host: "postgres", Port: 5432, Name: "munchkin",
		User: "munchkin_app", Password: config.Secret(senha),
		SSLMode: "disable", ConnectTimeout: 5_000_000_000,
	}

	dsn := db.DSN()
	assert.Contains(t, dsn, senha, "o driver precisa da senha de verdade")
	assert.Contains(t, dsn, "sslmode=disable")
	assert.Contains(t, dsn, "connect_timeout=5")

	// E o DSN nunca deve ser registrado: quem o imprimir, imprime a senha.
	assert.NotContains(t, strings.ToLower(dsn), "redigido")
}
