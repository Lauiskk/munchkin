// Package api carrega o contrato da API.
//
// O documento é EMBARCADO no binário, e não lido do disco: assim não há como
// servir uma versão e versionar outra, e a imagem distroless — sem shell e com
// sistema de arquivos somente leitura — continua servindo a documentação sem
// precisar de volume.
package api

import _ "embed"

// OpenAPI é o contrato da API, em OpenAPI 3.0.
//
//go:embed openapi.yaml
var OpenAPI []byte
