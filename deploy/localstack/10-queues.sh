#!/bin/bash
# Provisiona as filas do Munchkin no LocalStack.
#
# Roda em /etc/localstack/init/ready.d, ou seja, depois que o SQS está de pé.
# É idempotente: CreateQueue com os mesmos atributos devolve a fila existente,
# então reiniciar o container não quebra nada.
set -euo pipefail

REGIAO="${AWS_DEFAULT_REGION:-us-east-1}"
CONTA="000000000000"

arn() {
    echo "arn:aws:sqs:${REGIAO}:${CONTA}:$1"
}

# atributos monta o JSON de atributos da fila, já com a política de acesso.
#
# Em Python porque a política é um JSON DENTRO de um valor de JSON: montar isso
# à mão em shell, com três níveis de escape, é como se escreve um bug de aspas
# que só aparece em produção. O interpretador já está na imagem — o próprio
# LocalStack é escrito nele.
#
# Uso: atributos <arn> <acoes separadas por vírgula> [extras em JSON]
atributos() {
    python3 -c '
import json, sys
arn, acoes = sys.argv[1], sys.argv[2].split(",")
extras = json.loads(sys.argv[3]) if len(sys.argv) > 3 and sys.argv[3] else {}

# A política declara o princípio que o §2 do enunciado pede: o acesso à
# mensageria é controlado pelo BROKER, e não só por quem tem a credencial. Cada
# fila permite exatamente as ações de quem a usa — a de entrada é consumida, a
# de saída é publicada — e nada além disso.
politica = {
    "Version": "2012-10-17",
    "Statement": [{
        "Sid": "MunchkinSomenteAConta",
        "Effect": "Allow",
        "Principal": {"AWS": "arn:aws:iam::000000000000:root"},
        "Action": ["sqs:" + a for a in acoes],
        "Resource": arn,
    }],
}

# FIFO em todas: a ordem por carteira importa, e a deduplicação por
# MessageDeduplicationId é a segunda linha de defesa contra republicação.
#
# ContentBasedDeduplication fica DESLIGADO de propósito. Ligado, o SQS deduplica
# por hash do corpo — e dois eventos legítimos com o mesmo conteúdo em cinco
# minutos seriam engolidos. Quem manda passa o identificador explicitamente.
attrs = {
    "FifoQueue": "true",
    "ContentBasedDeduplication": "false",
    "Policy": json.dumps(politica, separators=(",", ":")),
}
attrs.update(extras)
print(json.dumps(attrs))
' "$@"
}

fila() {
    awslocal sqs create-queue --queue-name "$1" --attributes "$2" --region "$REGIAO" >/dev/null
    echo "fila pronta: $1"
}

# A DLQ primeiro: o redrive da fila principal precisa do ARN dela.
#
# Recebe do redrive e da aplicação, e é lida por quem for diagnosticar — por
# isso ela também admite ReceiveMessage e DeleteMessage.
fila "wager-transactions-dlq.fifo" \
    "$(atributos "$(arn wager-transactions-dlq.fifo)" \
        "SendMessage,ReceiveMessage,DeleteMessage,GetQueueAttributes" \
        '{"MessageRetentionPeriod":"1209600"}')"

DLQ_ARN="$(arn wager-transactions-dlq.fifo)"
REDRIVE="{\"deadLetterTargetArn\":\"${DLQ_ARN}\",\"maxReceiveCount\":\"5\"}"

# Fila de entrada: a aplicação CONSOME. Recebe, remove, e devolve a visibilidade
# no encerramento — daí ChangeMessageVisibility na lista.
#
# maxReceiveCount 5: quatro retentativas antes de desistir. Baixo o bastante
# para uma mensagem envenenada não ficar circulando, alto o bastante para
# absorver indisponibilidade transitória do banco.
fila "wager-transactions.fifo" \
    "$(atributos "$(arn wager-transactions.fifo)" \
        "SendMessage,ReceiveMessage,DeleteMessage,ChangeMessageVisibility,GetQueueAttributes" \
        "$(python3 -c 'import json,sys; print(json.dumps({"VisibilityTimeout":"30","RedrivePolicy":sys.argv[1]}))' "$REDRIVE")")"

# Destino dos eventos de saída: a aplicação só PUBLICA. Sem redrive — quem
# publica é o worker da outbox, e a retentativa dele vive na tabela, não na fila.
fila "wager-events.fifo" \
    "$(atributos "$(arn wager-events.fifo)" "SendMessage,GetQueueAttributes")"

awslocal sqs list-queues --region "$REGIAO"
