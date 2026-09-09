package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// runHealthCheck sonda o próprio serviço e devolve o código de saída.
//
// Existe para que a imagem do container não precise carregar curl ou wget só
// para responder ao HEALTHCHECK. A imagem final é distroless — sem shell e sem
// utilitários —, e acrescentar ferramenta de rede a ela seria abrir superfície
// de ataque para resolver um problema que o próprio binário resolve.
func runHealthCheck() int {
	// A porta é convertida para inteiro e conferida antes de entrar na URL.
	// O destino já é fixo em 127.0.0.1, mas passar a variável de ambiente
	// direto para a URL deixa uma string de origem externa no caminho — e um
	// valor malformado produziria um erro de rede confuso em vez de dizer que
	// a configuração está errada.
	port := 8080
	if raw := os.Getenv("HTTP_PORT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 65535 {
			fmt.Fprintf(os.Stderr, "HTTP_PORT inválida: %q\n", raw)
			return 1
		}
		port = parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// O gosec aponta G704 (SSRF) aqui porque a URL descende de uma variável de
	// ambiente, e está tecnicamente certo quanto à origem. O destino, porém, é
	// 127.0.0.1 fixo no literal, e a única parte variável é um inteiro já
	// conferido na faixa de portas — nenhuma string externa alcança a URL.
	// Quem controla o ambiente do processo já controla o processo.
	target := fmt.Sprintf("http://127.0.0.1:%d/health/live", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil) //nolint:gosec // G704: destino em loopback, porta validada
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: mesma URL em loopback do comentário acima
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
