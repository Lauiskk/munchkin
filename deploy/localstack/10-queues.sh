#!/bin/bash
# Provisiona as filas do Munchkin no LocalStack.
#
# Roda em /etc/localstack/init/ready.d, ou seja, depois que o SQS está de pé.
# É idempotente: CreateQueue com os mesmos atributos devolve a fila existente,
# então reiniciar o container não quebra nada.
set -euo pipefail

REGIAO="${AWS_DEFAULT_REGION:-us-east-1}"
CONTA="000000000000"

fila() {
    awslocal sqs create-queue --queue-name "$1" --attributes "$2" --region "$REGIAO" >/dev/null
    echo "fila pronta: $1"
}

arn() {
    echo "arn:aws:sqs:${REGIAO}:${CONTA}:$1"
}

# FIFO em todas: a ordem por carteira importa, e a deduplicação por
# MessageDeduplicationId é a segunda linha de defesa contra republicação.
#
# ContentBasedDeduplication fica DESLIGADO de propósito. Ligado, o SQS deduplica
# por hash do corpo — e dois eventos legítimos com o mesmo conteúdo em cinco
# minutos seriam engolidos. Quem manda passa o identificador explicitamente.
COMUM='"FifoQueue":"true","ContentBasedDeduplication":"false"'

# A DLQ primeiro: o redrive da fila principal precisa do ARN dela.
fila "wager-transactions-dlq.fifo" "{${COMUM},\"MessageRetentionPeriod\":\"1209600\"}"

DLQ_ARN="$(arn wager-transactions-dlq.fifo)"

# maxReceiveCount 5: quatro retentativas antes de desistir. Baixo o bastante
# para uma mensagem envenenada não ficar circulando, alto o bastante para
# absorver indisponibilidade transitória do banco.
fila "wager-transactions.fifo" \
    "{${COMUM},\"VisibilityTimeout\":\"30\",\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"${DLQ_ARN}\\\",\\\"maxReceiveCount\\\":\\\"5\\\"}\"}"

# Destino dos eventos de saída. Sem redrive: quem publica é o worker da outbox,
# e a retentativa dele vive na tabela, não na fila.
fila "wager-events.fifo" "{${COMUM}}"

awslocal sqs list-queues --region "$REGIAO"
