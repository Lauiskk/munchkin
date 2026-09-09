# Imagem do Munchkin.
#
# A versão do Go é a mesma declarada no go.mod, como o enunciado exige.

FROM golang:1.27.1-bookworm AS build

WORKDIR /src

# As dependências vêm antes do código para que a camada de download seja
# reaproveitada enquanto go.mod e go.sum não mudarem.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO desligado produz binário estático, requisito para a imagem distroless.
# -trimpath tira o caminho de compilação do binário, que de outro modo revelaria
# a estrutura de diretórios da máquina de build nas mensagens de erro.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# A imagem final não tem shell, gerenciador de pacotes nem utilitário de rede.
# Não há o que executar além do binário, então uma execução remota de comando
# não encontra ferramenta nenhuma para encadear.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/api /app/api

# nonroot é o usuário 65532 da própria imagem. O projeto de referência da
# equipe declarava USER root numa distroless, o que anula o ganho da imagem.
USER nonroot:nonroot

EXPOSE 8080

# A sonda é o próprio binário. Sem isso a imagem precisaria de curl ou wget,
# e acrescentar ferramenta de rede a uma distroless desfaz o motivo de usá-la.
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/app/api", "healthcheck"]

ENTRYPOINT ["/app/api"]
