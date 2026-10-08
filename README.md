# ✂️ encurtador-go

[![CI](https://github.com/thyagodev021/encurtador-go/actions/workflows/ci.yml/badge.svg)](https://github.com/thyagodev021/encurtador-go/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white)
![Deps](https://img.shields.io/badge/depend%C3%AAncias-0-brightgreen)
![License](https://img.shields.io/badge/license-MIT-green)

Encurtador de links com **API REST** em Go, usando **apenas a biblioteca padrão** (zero dependências).
Foi pensado para mostrar os pontos que importam em um serviço de verdade: concorrência segura,
persistência em disco, rate limiting, métricas e encerramento gracioso.

## ✨ O que tem

| Recurso | Como funciona |
|---|---|
| **Concorrência** | Armazenamento em 16 *shards* com `RWMutex`; o redirecionamento (caminho mais quente) usa só `RLock` + contadores atômicos |
| **Persistência** | Log *append-only* (JSON Lines) reaplicado ao iniciar, com **compactação atômica** (arquivo temporário + `rename`) |
| **Tolerância a falhas** | Linha truncada por queda de energia é detectada e ignorada, sem corromper o registro seguinte |
| **Rate limiting** | *Token bucket* por IP nas rotas de escrita, com `429` + `Retry-After` |
| **IDs sem viés** | `crypto/rand` + rejeição de bytes ≥ 248, evitando o viés de módulo do `byte % 62` |
| **Expiração (TTL)** | Links com validade: `410 Gone` ao expirar; limpeza periódica em segundo plano |
| **Segurança** | Só aceita `http(s)`; rejeita `javascript:`/`data:`, credenciais na URL (`user:senha@`) e links para o próprio serviço |
| **Observabilidade** | Logs estruturados (`log/slog`), `X-Request-ID`, `/healthz` e `/metrics` no formato Prometheus (com histograma de latência) |
| **Encerramento gracioso** | `SIGTERM` conclui as requisições em andamento, compacta o log e grava os cliques |

## 🚀 Como rodar

Precisa do **Go 1.22+** (baixe em [go.dev/dl](https://go.dev/dl)).

```bash
git clone https://github.com/thyagodev021/encurtador-go.git
cd encurtador-go
go run ./cmd/server                    # sobe em http://localhost:8080
```

Com Docker:

```bash
docker build -t encurtador-go .
docker run -p 8080:8080 -v encurtador-data:/data encurtador-go
```

### Opções

| Flag | Padrão | Descrição |
|---|---|---|
| `-addr` | `:8080` | endereço de escuta (env `ADDR`) |
| `-base-url` | `http://localhost:8080` | URL pública usada nos links curtos (env `BASE_URL`) |
| `-data` | `data/links.jsonl` | arquivo de dados; vazio = só memória (env `DATA_FILE`) |
| `-rate` / `-burst` | `5` / `10` | escritas por segundo por IP e rajada máxima (`-rate 0` desliga) |
| `-id-length` | `7` | tamanho dos IDs gerados |
| `-compact-every` | `10m` | intervalo entre compactações do log |
| `-sweep-every` | `1m` | intervalo da limpeza de links expirados |
| `-trust-proxy` | `false` | confiar no `X-Forwarded-For` (só atrás de um proxy seu) |
| `-sync` | `false` | `fsync` a cada escrita (mais seguro, mais lento) |
| `-log-format` | `text` | `text` ou `json` (env `LOG_FORMAT`) |

## 📡 API

| Método e rota | O que faz | Respostas |
|---|---|---|
| `POST /api/links` | cria um link | `201`, `400`, `409` (alias em uso), `413`, `429` |
| `GET /{id}` | redireciona (`302`) e conta o clique | `302`, `404`, `410` (expirado) |
| `GET /api/links/{id}` | estatísticas do link | `200`, `404`, `410` |
| `GET /api/links?limit=10` | links mais clicados (1 a 100) | `200`, `400` |
| `DELETE /api/links/{id}` | apaga o link | `204`, `404`, `429` |
| `GET /healthz` | saúde do serviço | `200` |
| `GET /metrics` | métricas no formato Prometheus | `200` |

Corpo do `POST /api/links`: `url` (obrigatório), `alias` (opcional, 3 a 32 caracteres `A-Za-z0-9_-`) e
`ttl_seconds` (opcional, `0` = não expira).

```console
$ curl -i -X POST localhost:8080/api/links -d '{"url":"https://go.dev/doc/"}'
HTTP/1.1 201 Created
Location: /api/links/r98vZoN
{"id":"r98vZoN","url":"https://go.dev/doc/","clicks":0,"created_at":"2026-10-07T08:43:44.899Z","short_url":"http://localhost:8080/r98vZoN"}

$ curl -X POST localhost:8080/api/links -d '{"url":"https://github.com/thyagodev021","alias":"meu-github","ttl_seconds":3600}'
{"id":"meu-github", ... ,"expires_at":"2026-10-07T09:43:44.907Z","short_url":"http://localhost:8080/meu-github"}

$ curl -s -o /dev/null -w "%{http_code} %{redirect_url}\n" localhost:8080/meu-github
302 https://github.com/thyagodev021

$ curl -X POST localhost:8080/api/links -d '{"url":"javascript:alert(1)"}'
{"error":"a URL deve começar com http:// ou https://"}
```

Excedeu o limite? A resposta é `429 Too Many Requests` com `Retry-After: 1`.

## 🧠 Decisões de projeto

**Locks fragmentados e ordem fixa.** Os links ficam em 16 mapas, cada um com seu `RWMutex`, para que operações em IDs
diferentes não disputem o mesmo lock. A ordem de aquisição é sempre `persist → shard → log`, o que evita deadlock.
A compactação toma o lock `persist` de forma exclusiva e, por isso, nenhuma criação ou remoção pode "escapar" do snapshot.

**Redirect sem escrita no disco.** Contar o clique é só um `atomic.Add`. Gravar uma linha no log a cada clique faria o
serviço mais lento e o arquivo crescer sem parar. O custo dessa escolha está na tabela de durabilidade abaixo.

**302, não 301.** O `301` seria cacheado pelo navegador e os cliques seguintes nem chegariam ao servidor.
Requisições `HEAD` também não contam clique (prefetch e crawlers não inflam as estatísticas).

**Compactação atômica.** O estado é escrito em `links.jsonl.tmp`, sincronizado com `fsync` e só então trocado por
`rename`. Se o processo morrer no meio, o arquivo antigo continua intacto.

### Durabilidade, sem rodeios

| Evento | Links criados/apagados | Cliques |
|---|---|---|
| Encerramento normal (`SIGTERM`/`Ctrl+C`) | ✅ preservados | ✅ preservados |
| Queda brusca (`kill -9`, falta de energia) | ✅ preservados (com `-sync`, cada escrita é gravada com `fsync`) | ⚠️ perde-se o que veio **depois da última compactação** (até 10 min por padrão) |

Os dois cenários estão cobertos em `internal/store/store_test.go` e também foram conferidos manualmente com o binário real:
após `SIGTERM`, 5 cliques voltaram depois de reiniciar; após `kill -9`, o link novo sobreviveu e os 3 cliques recentes se perderam.

## ✅ Testes

```bash
go test -race -count=1 ./...        # 58 testes, com o detector de race
make cover                          # cobertura
make bench                          # benchmarks
```

Cobertura: `ratelimit` 97%, `server` 93%, `shortid` 93%, `store` 87%. Destaques:

- **Concorrência**: 64 goroutines × 2.000 cliques no mesmo link (contagem exata) e 100 goroutines criando o mesmo alias (exatamente 1 vence);
- **Operações misturadas durante compactações** (criar, visitar, apagar e compactar ao mesmo tempo), conferindo o estado depois de reabrir;
- **Falhas de disco simuladas**: última linha truncada, linhas de lixo, queda sem `Close`;
- **Relógio falso** (`testclock`) para testar expiração e rate limit sem `sleep`;
- **Teste estatístico** que detecta o viés de módulo no gerador de IDs.

> 💡 Para garantir que os testes pegam bugs de verdade, introduzi defeitos de propósito (viés no gerador de IDs, `HEAD` contando clique,
> compactação sem gravar cliques) e confirmei que cada um fazia algum teste falhar.

## ⏱️ Benchmarks

Medidos em uma máquina de **1 núcleo** (Xeon 2,1 GHz); em hardware com mais núcleos os benchmarks paralelos escalam melhor.

| Operação | Tempo |
|---|---|
| `Store.Visit` (caminho quente do redirect) | ~210 ns |
| `Store.Put` em memória | ~1,1 µs |
| `shortid.New(7)` | ~560 ns |
| Redirect completo pelo handler HTTP (inclui roteamento, middlewares, log e métricas) | ~7,7 µs |

## 📁 Estrutura

```
cmd/server/            main: flags, workers em segundo plano, shutdown gracioso
internal/store/        armazenamento concorrente + log append-only + compactação
internal/server/       rotas, validação, middlewares (request-id, recover, log) e métricas
internal/ratelimit/    token bucket por chave
internal/shortid/      IDs base62 sem viés
internal/testclock/    relógio manual para testes
```

## 🗺️ Roteiro

- [ ] Gravar cliques em lote (ex.: a cada 5 s) para reduzir a janela de perda em quedas bruscas
- [ ] Backend de armazenamento alternativo (SQLite/Postgres) atrás da mesma interface
- [ ] Autenticação por API key e *links por usuário*
- [ ] Página web simples para criar links


