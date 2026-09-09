// Package migrations carrega as migrations para dentro do binário.
//
// Embarcar em vez de montar um diretório significa que a imagem é
// autossuficiente: não há como o container subir com uma versão do código e
// outra das migrations, que é uma das formas mais desagradáveis de um deploy
// dar errado.
package migrations

import "embed"

// FS contém os arquivos .sql versionados deste diretório.
//
//go:embed *.sql
var FS embed.FS
