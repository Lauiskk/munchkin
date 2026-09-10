// Carga do Munchkin.
//
// Três perfis, medindo coisas diferentes. Rodá-los juntos misturaria efeitos:
// a contenção de uma carteira só inflaria a latência das carteiras distintas, e
// o relatório não diria de onde veio o quê.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE = __ENV.BASE || 'http://127.0.0.1:8080';
const METRICS = __ENV.METRICS || 'http://127.0.0.1:9092';
const KEYCLOAK = __ENV.KEYCLOAK || 'http://127.0.0.1:8180';

// Contadores próprios. As recusas de negócio NÃO entram na taxa de erro: uma
// aposta recusada por saldo é o sistema funcionando, e contá-la como falha
// tornaria o perfil de contenção um relatório de desastre.
const processadas = new Counter('munchkin_processadas');
const recusadas = new Counter('munchkin_recusadas_por_saldo');
const conflitos = new Counter('munchkin_conflitos_idempotencia');
const replays = new Counter('munchkin_replays');
const falhas = new Counter('munchkin_falhas');
// Latência POR PERFIL. É o que prova a contenção: a mesma operação numa
// carteira disputada custa mais que numa carteira só sua, e a diferença entre
// as duas curvas é o preço do lock de linha. Recusa por saldo não prova nada
// disso — prova só que a carteira acabou.
const latDistintas = new Trend('munchkin_lat_distintas', true);
const latContencao = new Trend('munchkin_lat_contencao', true);
const latReplay = new Trend('munchkin_lat_replay', true);

export const options = {
  scenarios: {
    carteiras_distintas: {
      executor: 'constant-vus', vus: 20, duration: '30s',
      exec: 'carteirasDistintas', startTime: '0s',
    },
    mesma_carteira: {
      executor: 'constant-vus', vus: 20, duration: '30s',
      exec: 'mesmaCarteira', startTime: '35s',
    },
    replay: {
      executor: 'constant-vus', vus: 10, duration: '20s',
      exec: 'replay', startTime: '70s',
    },
  },
  // p99 não vem por padrão, e é justamente a cauda que interessa num caminho
  // que toma lock de linha.
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  // O teardown mede quanto a outbox leva para drenar depois da carga, e o
  // padrão de 60s do k6 é menor que a própria drenagem — o que fazia a medição
  // morrer justamente quando ela ficava interessante.
  teardownTimeout: '240s',
  thresholds: {
    // Falha de verdade — 5xx ou rede — é o que não pode acontecer. Recusa de
    // negócio e conflito de idempotência são desfechos previstos.
    munchkin_falhas: ['count==0'],
  },
};

function token(clientId, secret) {
  const r = http.post(`${KEYCLOAK}/realms/munchkin/protocol/openid-connect/token`,
    { grant_type: 'client_credentials', client_id: clientId, client_secret: secret },
    { headers: { 'Content-Type': 'application/x-www-form-urlencoded' } });
  if (r.status !== 200) {
    throw new Error(`Keycloak não devolveu token (${r.status}). A pilha está no ar?`);
  }
  return r.json('access_token');
}

function abrirCarteira(admin, saldo) {
  const jogador = uuidv4();
  const r = http.post(`${BASE}/wallets`,
    JSON.stringify({ playerId: jogador, initialBalance: { amount: saldo, currency: 'BRL' } }),
    { headers: { Authorization: `Bearer ${admin}`, 'Content-Type': 'application/json' } });
  if (r.status !== 201) {
    throw new Error(`abertura de carteira falhou (${r.status}): ${r.body}`);
  }
  return { id: r.json('id'), jogador };
}

export function setup() {
  const admin = token('wallet-admin', 'local-only-wallet-admin');
  const provedor = token('provider-a', 'local-only-provider-a');

  // Carteira ÚNICA e generosa para o perfil de contenção: o objetivo ali é medir
  // o custo do lock, e uma carteira que zera cedo mediria só recusa.
  const contenciosa = abrirCarteira(admin, '1000000.00');

  // Carteira do replay, com uma operação já processada para reenviar.
  const doReplay = abrirCarteira(admin, '1000.00');
  const externo = `replay-${uuidv4()}`;
  http.post(`${BASE}/wagering/transactions`,
    corpoDeAposta(externo, doReplay, '10.00'),
    cabecalhos(provedor, externo));

  return { admin, provedor, contenciosa, doReplay, externo };
}

function corpoDeAposta(externo, carteira, valor) {
  return JSON.stringify({
    providerId: 'provider-a', externalTransactionId: externo,
    playerId: carteira.jogador, walletId: carteira.id,
    roundId: 'carga', gameId: 'fortune-chimp',
    kind: 'BET', money: { amount: valor, currency: 'BRL' },
  });
}

function cabecalhos(provedor, externo) {
  return {
    headers: {
      Authorization: `Bearer ${provedor}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': `provider-a:${externo}`,
    },
    tags: { name: 'POST /wagering/transactions' },
  };
}

// classificar separa desfecho previsto de falha, e alimenta o atraso da outbox.
function classificar(r) {
  if (r.status === 200) {
    r.json('idempotentReplay') ? replays.add(1) : processadas.add(1);
    return true;
  }
  if (r.status === 422) { recusadas.add(1); return true; }
  if (r.status === 409) { conflitos.add(1); return true; }
  falhas.add(1);
  return false;
}

export function carteirasDistintas(dados) {
  // Uma carteira por iteração: sem contenção, isto mede a vazão do caminho
  // financeiro inteiro — lock, ledger, outbox e commit.
  const carteira = abrirCarteira(dados.admin, '1000.00');
  const externo = `dist-${uuidv4()}`;
  const r = http.post(`${BASE}/wagering/transactions`,
    corpoDeAposta(externo, carteira, '10.00'), cabecalhos(dados.provedor, externo));
  latDistintas.add(r.timings.duration);
  check(r, { 'distinta: desfecho previsto': classificar });
}

export function mesmaCarteira(dados) {
  // TODOS os VUs na mesma carteira: mede o custo do lock de linha, que é a
  // decisão central de concorrência deste sistema.
  const externo = `cont-${uuidv4()}`;
  const r = http.post(`${BASE}/wagering/transactions`,
    corpoDeAposta(externo, dados.contenciosa, '1.00'), cabecalhos(dados.provedor, externo));
  latContencao.add(r.timings.duration);
  check(r, { 'contenção: desfecho previsto': classificar });
}

export function replay(dados) {
  // A MESMA operação, sempre: mede o caminho de reenvio, que é o que um
  // provedor faz quando não recebeu a resposta.
  const r = http.post(`${BASE}/wagering/transactions`,
    corpoDeAposta(dados.externo, dados.doReplay, '10.00'),
    cabecalhos(dados.provedor, dados.externo));
  latReplay.add(r.timings.duration);
  check(r, { 'replay: desfecho previsto': classificar });
}

// amostrarOutbox lê o medidor de atraso durante a carga.
//
// Durante, e não depois: o publicador roda a cada dois segundos e a fila esvazia
// em poucos segundos após a carga cessar. Medir no fim mediria sempre zero.
export function handleSummary(dados) {
  return { stdout: resumo(dados) };
}

export function teardown() {
  const atraso = lerAtrasoDaOutbox();
  console.log(`atraso da outbox ao fim da carga: ${atraso.toFixed(1)}s`);

  // Quanto tempo a fila leva para drenar depois que a carga cessa. É o número
  // que diz se o atraso era acúmulo temporário ou incapacidade permanente.
  const inicio = Date.now();
  let ultimo = atraso;
  while (Date.now() - inicio < 200000) {
    sleep(5);
    ultimo = lerAtrasoDaOutbox();
    if (ultimo === 0) {
      console.log(`outbox drenada em ${((Date.now() - inicio) / 1000).toFixed(0)}s após a carga`);
      return;
    }
  }
  console.log(`outbox AINDA com ${ultimo.toFixed(1)}s de atraso 200s após a carga`);
}

function lerAtrasoDaOutbox() {
  const r = http.get(`${METRICS}/metrics`);
  const linha = r.body.split('\n')
    .find((l) => l.startsWith('munchkin_outbox_pending_age_seconds'));
  return linha ? parseFloat(linha.split(' ')[1]) : 0;
}

function perfil(m, nome) {
  const v = (m[nome] && m[nome].values) || {};
  const c = (x) => `${(v[x] || 0).toFixed(0)}ms`.padStart(9);
  return `${c('med')}  ${c('p(95)')}  ${c('p(99)')}  ${c('max')}`;
}

function resumo(dados) {
  const m = dados.metrics;
  const n = (nome, campo) => (m[nome] && m[nome].values[campo] !== undefined
    ? m[nome].values[campo] : 0);
  const ms = (v) => `${v.toFixed(1)}ms`;

  return `
═══════════════════════════════════════════════════════════════
  MUNCHKIN — CARGA
═══════════════════════════════════════════════════════════════

  Requisições         ${n('http_reqs', 'count')}
  Vazão               ${n('http_reqs', 'rate').toFixed(1)} req/s

  Latência, por perfil          p50        p95        p99        máx
    carteiras distintas     ${perfil(m, 'munchkin_lat_distintas')}
    MESMA carteira          ${perfil(m, 'munchkin_lat_contencao')}
    replay idempotente      ${perfil(m, 'munchkin_lat_replay')}

  Agregado                  ${perfil(m, 'http_req_duration')}

  Desfechos
    processadas       ${n('munchkin_processadas', 'count')}
    replays           ${n('munchkin_replays', 'count')}
    recusadas (saldo) ${n('munchkin_recusadas_por_saldo', 'count')}
    conflitos (409)   ${n('munchkin_conflitos_idempotencia', 'count')}
    FALHAS            ${n('munchkin_falhas', 'count')}

  Recusa por saldo e conflito de idempotência são desfechos previstos, não
  erros. Só "FALHAS" conta como falha — 5xx ou rede.
═══════════════════════════════════════════════════════════════
`;
}
