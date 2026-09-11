package ops

import (
	"regexp"
	"strings"
	"testing"
)

// TestTodoRecursoExternoDaDocTemIntegridade guarda o `integrity` dos arquivos
// que a página de documentação busca fora.
//
// A versão fixada diz QUAL arquivo pedir; o hash diz que foi ELE que chegou.
// Sem o segundo, quem abre a página executa o que o CDN devolver — e o risco não
// aparece em nenhum teste funcional, porque a página continua "funcionando".
//
// Este caso existe sobretudo para o dia em que alguém subir a versão do Swagger
// UI: trocar a URL e esquecer o hash deixa a página em branco, e trocar a URL e
// REMOVER o hash reabre o buraco em silêncio. Aqui, as duas coisas falham.
func TestTodoRecursoExternoDaDocTemIntegridade(t *testing.T) {
	externos := regexp.MustCompile(`(?:src|href)="(https?://[^"]+)"`).
		FindAllStringSubmatch(paginaDocs, -1)

	if len(externos) == 0 {
		t.Fatal("nenhum recurso externo encontrado: o teste perdeu o alvo e passaria vazio")
	}

	// Cada tag que busca algo de fora precisa carregar o hash e o crossorigin —
	// o navegador só confere integridade de recurso servido com CORS.
	for _, tags := range regexp.MustCompile(`<(?:script|link)[^>]*>`).FindAllString(paginaDocs, -1) {
		if !strings.Contains(tags, "http") {
			continue
		}
		if !strings.Contains(tags, "integrity=\"sha384-") {
			t.Errorf("recurso externo sem integrity:\n%s", tags)
		}
		if !strings.Contains(tags, `crossorigin="anonymous"`) {
			t.Errorf("recurso externo sem crossorigin, o hash não seria conferido:\n%s", tags)
		}
	}
}

// TestPaginaDeDocNaoInterpolaNada garante que a página continue sendo constante.
//
// Ela é o único HTML que o serviço emite. Enquanto for texto fixo, não há por
// onde entrar XSS; no dia em que alguém montar um pedaço dela a partir de
// entrada do usuário, o risco nasce sem que nada mais no projeto perceba.
func TestPaginaDeDocNaoInterpolaNada(t *testing.T) {
	for _, marca := range []string{"%s", "%v", "%d", "{{", "${"} {
		if strings.Contains(paginaDocs, marca) {
			t.Errorf("a página de documentação passou a interpolar %q: "+
				"se a origem for entrada de usuário, isso é XSS", marca)
		}
	}
}
