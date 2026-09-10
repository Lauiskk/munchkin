//go:build integration

// Package recovery_test verifica o que só aparece com processos de verdade.
//
// Os testes de concorrência exercitam goroutines num processo só: provam o lock
// de linha e a coordenação no banco, mas não provam ausência de dependência de
// instância única — porque um processo compartilha memória consigo mesmo. O §14
// trata essa dependência como eliminatória, e a única prova é rodar o binário
// compilado três vezes, com pools, memória e ciclos de vida separados.
package recovery_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/tests/infra"
)

const (
	realm           = "munchkin"
	audience        = "munchkin-api"
	clientProviderA = "provider-a"
	secretProviderA = "local-only-provider-a"
	clientAdmin     = "wallet-admin"
	secretAdmin     = "local-only-wallet-admin"
)

// diario acumula a saída do processo com exclusão mútua.
//
// O `exec` escreve de outra goroutine enquanto o teste lê. Com um bytes.Buffer
// cru isso é corrida — e ela se manifesta do jeito mais confuso possível: a
// leitura devolve vazio, e a falha do processo aparece como "não ficou pronta",
// sem dizer por quê.
type diario struct {
	mu    sync.Mutex
	texto strings.Builder
}

func (d *diario) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.texto.Write(p)
}

func (d *diario) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.texto.String()
}

// instancia é um processo da aplicação.
type instancia struct {
	nome    string
	cmd     *exec.Cmd
	baseURL string
	saida   *diario
	// morreu fecha quando o processo termina, com ou sem erro.
	morreu chan struct{}
	saiu   error
}

// ambiente reúne a infraestrutura e as instâncias.
type ambiente struct {
	instancias []*instancia
	admin      string
	provedor   string
	pilha      infra.Stack
	t          *testing.T
}

// portaLivre reserva uma porta e a devolve.
//
// Há uma corrida teórica entre fechar e o processo abrir. Na prática ela não
// acontece nesta suíte, e a alternativa — porta fixa — falha exatamente quando o
// ambiente local está no ar, que é sempre.
func portaLivre(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	porta := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return porta
}

// binario compila a aplicação uma vez para toda a suíte.
var (
	binarioUmaVez sync.Once
	binarioPath   string
	binarioDir    string
	binarioErr    error
)

func compilar(t *testing.T) string {
	t.Helper()
	binarioUmaVez.Do(func() {
		raiz, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			binarioErr = err
			return
		}
		// Diretório da SUÍTE, não do teste: t.TempDir() é apagado quando aquele
		// teste termina, e o segundo cenário encontraria o binário sumido —
		// falhando como "não iniciou", que não diz nada sobre a causa.
		dir, err := os.MkdirTemp("", "munchkin-recovery-")
		if err != nil {
			binarioErr = err
			return
		}
		binarioDir = dir
		destino := filepath.Join(dir, "api")

		// O BINÁRIO, e não `go run`: o §13 pede instâncias independentes, e
		// `go run` acrescenta um processo pai que confunde o encerramento —
		// matar o pai não mata o filho, e o teste passaria a medir outra coisa.
		cmd := exec.Command("go", "build", "-o", destino, "./cmd/api")
		cmd.Dir = raiz
		if saida, err := cmd.CombinedOutput(); err != nil {
			binarioErr = fmt.Errorf("compilação falhou: %v\n%s", err, saida)
			return
		}
		binarioPath = destino
	})
	require.NoError(t, binarioErr)
	return binarioPath
}

// subir inicia um processo da aplicação e espera ele ficar pronto.
func (a *ambiente) subir(t *testing.T, nome string, env map[string]string) *instancia {
	t.Helper()

	porta := portaLivre(t)
	completo := os.Environ()
	for k, v := range env {
		completo = append(completo, k+"="+v)
	}
	completo = append(completo,
		fmt.Sprintf("HTTP_PORT=%d", porta),
		fmt.Sprintf("METRICS_PORT=%d", portaLivre(t)),
	)

	saida := &diario{}
	cmd := exec.Command(compilar(t))
	cmd.Env = completo
	cmd.Stdout = saida
	cmd.Stderr = saida
	// Grupo próprio: permite matar o processo sem alcançar quem roda o teste.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	require.NoError(t, cmd.Start(), "instância %s não iniciou", nome)

	inst := &instancia{
		nome: nome, cmd: cmd, saida: saida, morreu: make(chan struct{}),
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", porta),
	}
	go func() {
		inst.saiu = cmd.Wait()
		close(inst.morreu)
	}()
	a.instancias = append(a.instancias, inst)
	t.Cleanup(func() { inst.matar() })

	esperarPronta(t, inst)
	return inst
}

// esperarPronta bloqueia até a instância responder que está pronta.
func esperarPronta(t *testing.T, inst *instancia) {
	t.Helper()
	prazo := time.Now().Add(90 * time.Second)

	for time.Now().Before(prazo) {
		select {
		case <-inst.morreu:
			t.Fatalf("instância %s morreu antes de ficar pronta (%v):\n%s",
				inst.nome, inst.saiu, inst.log())
		default:
		}

		resp, err := http.Get(inst.baseURL + "/health/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("instância %s não ficou pronta em 90s:\n%s", inst.nome, inst.log())
}

func (i *instancia) log() string { return i.saida.String() }

// matar derruba o processo sem dar chance de limpar nada.
//
// SIGKILL, e não SIGTERM: o encerramento ordenado já tem teste próprio. O que
// interessa aqui é o desligamento que não coopera — o único que prova que a
// recuperação não depende de o processo morto ter feito alguma coisa.
func (i *instancia) matar() {
	if i.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-i.cmd.Process.Pid, syscall.SIGKILL)
	<-i.morreu
}

// token obtém um access token real do Keycloak.
func token(t *testing.T, base, clientID, secret string) string {
	t.Helper()

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	resp, err := http.PostForm(base+"/realms/"+realm+"/protocol/openid-connect/token",
		form)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var corpo struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&corpo))
	require.NotEmpty(t, corpo.AccessToken)
	return corpo.AccessToken
}

// chamar faz uma requisição autenticada contra uma instância.
func chamar(t *testing.T, inst *instancia, metodo, caminho, tok, corpo string,
	cabecalhos map[string]string) (int, []byte) {
	t.Helper()

	var leitor io.Reader
	if corpo != "" {
		leitor = strings.NewReader(corpo)
	}
	req, err := http.NewRequestWithContext(context.Background(), metodo,
		inst.baseURL+caminho, leitor)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	if corpo != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range cabecalhos {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	bruto, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, bruto
}

// TestMain derruba a infraestrutura compartilhada ao fim da suíte.
func TestMain(m *testing.M) {
	codigo := m.Run()
	infra.Stop()
	if binarioDir != "" {
		_ = os.RemoveAll(binarioDir)
	}
	os.Exit(codigo)
}

// externo devolve um identificador de operação único para este teste.
//
// A pilha é compartilhada entre os cenários, então "tx-1" em dois testes é a
// MESMA operação para o sistema — e o segundo receberia conflito de
// idempotência. O prefixo por teste é o que mantém os cenários independentes
// sem precisar de um banco por cenário.
func externo(t *testing.T, sufixo string) string {
	t.Helper()
	return strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + sufixo
}
