#!/bin/bash
# Provisionamento dos papéis do Munchkin.
#
# Dois papéis, e a separação não é cerimônia: o enunciado exige que a
# imutabilidade do ledger seja imposta pelo banco. Em PostgreSQL, o DONO de uma
# tabela tem privilégio por ownership, não por concessão — revogar UPDATE e
# DELETE do dono não adianta, porque ele pode se reconceder a qualquer momento.
# A proteção só vale se a aplicação conectar como um papel que NÃO é dono.
#
#   $POSTGRES_USER : dono do schema. Aplica migrations. A aplicação nunca usa.
#   munchkin_app   : papel da aplicação. Recebe só o que precisa, e nas
#                    migrations perde UPDATE e DELETE sobre os lançamentos.
#
# Roda uma única vez, na criação do volume. É o mesmo script usado pelos testes
# de integração: ambiente de teste mais permissivo que o de execução é como um
# teste verde acompanha uma aplicação quebrada.
set -euo pipefail

APP_USER="${MUNCHKIN_APP_USER:-munchkin_app}"
APP_PASSWORD="${MUNCHKIN_APP_PASSWORD:?MUNCHKIN_APP_PASSWORD não definida}"

# A senha entra numa instrução SQL por interpolação. Recusar aspas e barra
# invertida elimina a única forma de essa interpolação virar injeção. Em
# produção a credencial viria de um cofre, não de variável de ambiente.
if [[ "$APP_PASSWORD" == *"'"* || "$APP_PASSWORD" == *'\'* ]]; then
  echo "MUNCHKIN_APP_PASSWORD não pode conter aspa simples nem barra invertida" >&2
  exit 1
fi

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-SQL
	CREATE ROLE ${APP_USER} WITH LOGIN PASSWORD '${APP_PASSWORD}';

	GRANT CONNECT ON DATABASE ${POSTGRES_DB} TO ${APP_USER};
	GRANT USAGE ON SCHEMA public TO ${APP_USER};

	-- A aplicação não cria nem altera estrutura: isso é das migrations, que
	-- rodam como o dono.
	REVOKE CREATE ON SCHEMA public FROM ${APP_USER};
	REVOKE CREATE ON SCHEMA public FROM PUBLIC;

	-- Privilégios padrão para as tabelas que as migrations ainda vão criar.
	-- Sem isto, cada migration teria de lembrar de conceder acesso — e a que
	-- esquecesse só falharia em execução, não ao ser escrita.
	ALTER DEFAULT PRIVILEGES IN SCHEMA public
	  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ${APP_USER};

	ALTER DEFAULT PRIVILEGES IN SCHEMA public
	  GRANT USAGE, SELECT ON SEQUENCES TO ${APP_USER};
SQL

echo "papel ${APP_USER} criado"
