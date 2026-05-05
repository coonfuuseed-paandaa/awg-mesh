<!-- synced: 2026-05-05 source-commit: 2f22776a64f7c24bab9d2bc265a0a1458fa230e3 -->
[English](README.md) | **Русский** | [中文](README.zh-CN.md)

[![CI](https://github.com/coonfuuseed-paandaa/awg-mesh/actions/workflows/build.yml/badge.svg)](https://github.com/coonfuuseed-paandaa/awg-mesh/actions/workflows/build.yml)
[![Release](https://img.shields.io/github/v/release/coonfuuseed-paandaa/awg-mesh?logo=github)](https://github.com/coonfuuseed-paandaa/awg-mesh/releases)
[![Go 1.25](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![GHCR](https://img.shields.io/badge/GHCR-awg--mesh--node-2496ED?logo=docker)](https://github.com/coonfuuseed-paandaa/awg-mesh/pkgs/container/awg-mesh-node)
[![Docker Hub](https://img.shields.io/badge/Docker_Hub-awg--mesh--node-2496ED?logo=docker)](https://hub.docker.com/r/coonfuuseedpaandaa/awg-mesh-node)

# awg-mesh

Docker-native зашифрованная overlay-сеть на базе AmneziaWG. awg-mesh v2
использует плоскую role-tagged топологию, регистрацию через control plane,
локальный backup/restore, развёртывание MikroTik RouterOS через `/container`
и release gates, которые доказывают корректность кодовых контрактов и
поведение продукта до публикации тега.

## Обзор архитектуры

```mermaid
graph TB
    subgraph Admin["Admin workstation"]
        ctl["mesh-ctl"]
    end

    subgraph Control["Control plane"]
        cp["awg-mesh-node\n--mode control-plane"]
    end

    subgraph Mesh["v2 mesh nodes"]
        master["master + balancer"]
        egress["egress"]
        ingress["ingress"]
        client["client"]
        mt["MikroTik\n/container client"]
    end

    user["Users / apps"]
    internet["Internet"]

    ctl -- "mTLS admin gRPC" --> cp
    ctl -- "prepare local certs/tokens" --> client
    ctl -- "prepare local certs/tokens" --> master
    cp -- "registered identity\npeer/control data" --> master
    cp -- "registered identity\npeer/control data" --> egress
    cp -- "registered identity\npeer/control data" --> ingress
    cp -- "registered identity\npeer/control data" --> client
    cp -- "registered identity\npeer/control data" --> mt
    client -- "vanilla WG to master" --> master
    mt -- "vanilla WG to master" --> master
    master -- "mesh-internal AWG" --> egress
    ingress -- "service forwarding" --> client
    egress -- "NAT at boundary" --> internet
    user --> ingress
```

Файл топологии v2 объявляет узлы один раз в разделе `nodes:` и назначает
каждому узлу одну или несколько ролей:

| Роль | Назначение |
|---|---|
| `client` | Пользовательское устройство или домашний сервер. Эта роль эксклюзивна. |
| `master` | Принимает client links и мостит их в mesh. |
| `balancer` | Выбирает активный egress/mesh path для flow. |
| `egress` | Выполняет internet-bound masquerade на границе. |
| `ingress` | Публикует сервисы mesh clients через публичные hostnames или ports. |

Полное обоснование архитектуры v2 и инварианты смотрите в
[docs/architecture/F-009-overview.md](docs/architecture/F-009-overview.md).

## Что нового в v2.0

- **Топология schema v2** с `schema_version: 2`, role-tagged `nodes:` и
  объявлениями service ingress.
- **Role-agnostic CLI**: текущий onboarding использует `mesh-ctl node prepare`,
  `mesh-ctl node init`, `mesh-ctl node list` и `mesh-ctl node remove`.
  Legacy role subcommands `master`, `endpoint` и `client` удалены из v2 operator path.
- **Регистрация через control plane** с CA-backed mTLS и локальными admin
  certificates. Insecure control-plane admin paths не входят в release flow.
- **Локальный backup/restore** для admin state, topology и опциональных архивов
  control-plane state.
- **Развёртывание MikroTik RouterOS через `/container`** с помощью
  `mesh-ctl node prepare --platform mikrotik`. Runtime release validation
  целится в RouterOS 7.21+; generator syntax tests также покрывают 7.16.2 и 7.20.8.
- **Critical suite + product emulation playbook** как release blockers.
  Перед tagging требуется `PRODUCT_WORKS`.
- **Go module v2 path**:
  `github.com/coonfuuseed-paandaa/awg-mesh/v2`.

## Быстрый старт

Этот локальный walkthrough собирает инструменты, валидирует пример топологии v2
и подготавливает локальные node credentials. Сам по себе он не разворачивает
production mesh; для развёртывания нужны реальные hosts, Docker, доступная сеть
до control plane и node-specific firewall rules.

### Требования

- Go 1.25+
- Docker Engine 24+ для image builds и release simulations
- Linux или WSL2 для Bash release gates
- `/dev/net/tun` на hosts, где запускаются data-plane containers

### Установка mesh-ctl

Для текущей release line v2:

```bash
go install github.com/coonfuuseed-paandaa/awg-mesh/v2/cmd/mesh-ctl@v2.0.0
```

Для локальной разработки из clone:

```bash
CGO_ENABLED=1 go build -trimpath -o bin/mesh-ctl ./cmd/mesh-ctl
CGO_ENABLED=1 go build -trimpath -o bin/awg-mesh-node ./cmd/awg-mesh-node
```

Добавьте директорию с binary в `PATH`, затем проверьте версию:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
mesh-ctl version
```

### Создание и валидация топологии

```bash
mkdir -p .mesh-local
cp mesh-topology.example.yml .mesh-local/mesh-topology.yml
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml topology validate
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml node list
```

Ожидаемая форма:

```text
topology "mesh-topology.yml" valid: schema_version=2 ...
```

### Подготовка node material

`node prepare` пишет локальный admin-side state в `--config-dir`:

```bash
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml node prepare master-01
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml node prepare home-server-01
```

Созданные для каждого узла артефакты включают:

```text
.mesh-local/ca.crt
.mesh-local/nodes/<name>/token
.mesh-local/nodes/<name>/mesh.token
.mesh-local/nodes/<name>/node.crt
.mesh-local/nodes/<name>/node.key
```

Для развёртывания MikroTik RouterOS container укажите reachable control-plane address:

```bash
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml \
  node prepare mikrotik-home \
  --platform mikrotik \
  --control-plane 192.0.2.10:9090 \
  --target-ros 7.21.4
```

Контракт совместимости RouterOS описан в
[docs/MIKROTIK-VERSION-COMPAT.md](docs/MIKROTIK-VERSION-COMPAT.md).

### Регистрация узлов в control plane

Запустите control-plane node в deployment environment, затем зарегистрируйте
подготовленные nodes:

```bash
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml \
  node init master-01 --control-plane 192.0.2.10:9090

mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml \
  node init home-server-01 --control-plane 192.0.2.10:9090
```

`node init` использует локальный CA и подготовленный node certificate.
Отклонённая регистрация — blocker для deployment, а не warning.

## Пример топологии

Минимальная топология v2:

```yaml
schema_version: 2

mesh:
  name: example-mesh
  overlay_supernet: 172.21.92.0/24
  tenants: [default]

nodes:
  - name: master-01
    roles: [master, balancer, egress]
    overlay_ip: 172.21.92.2
    bridge_ip: 192.168.93.10
    public_ip: 203.0.113.10
    region: eu
    internet_iface: eth0

  - name: home-server-01
    roles: [client]
    overlay_ip: 172.21.92.130
    region: home
    preferred_master: master-01

services:
  - name: jellyfin
    owner_node: home-server-01
    protocol: tcp
    local_port: 8096
    tenant: default
    ingress:
      - hostname: media.example.com
        mode: sni_passthrough
        ingress_node: ingress-de-01
```

Используйте [mesh-topology.example.yml](mesh-topology.example.yml) как
поддерживаемую отправную точку.

## Docker-образы

Release images публикуются и в GHCR, и в Docker Hub.

```text
ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v2.0.0
ghcr.io/coonfuuseed-paandaa/awg-mesh-client:v2.0.0
ghcr.io/coonfuuseed-paandaa/awg-mesh:v2.0.0          # GHCR-only legacy node alias

docker.io/coonfuuseedpaandaa/awg-mesh-node:v2.0.0
docker.io/coonfuuseedpaandaa/awg-mesh-client:v2.0.0
```

CI workflow публикует multi-arch manifests для:

```text
linux/amd64
linux/386
linux/arm64
linux/arm/v7
linux/arm/v6
```

При release tag workflow публикует теги `vX.Y.Z`, `X.Y.Z`, `X.Y`, `X` и
commit-SHA, где major alias включён для non-v0 releases. Для production
фиксируйте `:vX.Y.Z`; используйте `:latest` только для preview environments.

## Команды

### Топология

```bash
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml topology validate
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml topology validate --output json
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml topology generate-prometheus-config
mesh-ctl migrate --from old-v1-topology.yml --to mesh-topology.yml
```

### Жизненный цикл node

```bash
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml node list
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml node prepare <name>
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml node init <name> --control-plane <host:port>
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml node remove <name> --control-plane <host:port>
```

### Резервное копирование и восстановление

```bash
mesh-ctl --topology mesh-topology.yml --config-dir ~/.mesh-ctl backup awg-mesh-backup.zip
mesh-ctl --topology restored-topology.yml --config-dir ~/.mesh-ctl-restored restore awg-mesh-backup.zip --confirm
```

### Планирование upgrade

```bash
mesh-ctl --config-dir .mesh-local --topology mesh-topology.yml upgrade v2.0.1 --dry-run
mesh-ctl upgrade status
mesh-ctl upgrade pause
mesh-ctl upgrade resume
```

Выполнение v2 upgrade намеренно заблокировано, пока не появится v2 deploy executor.
Текущая поддержка upgrade — это поверхность plan/state management, а не
автоматический production rollout.

### Ротация и аудит

```bash
mesh-ctl rotate --mesh-wide --tier 1 --control-plane <host:port>
mesh-ctl rotate --mesh-wide --tier 2 --control-plane <host:port>
mesh-ctl rotate --mesh-wide --tier 3 --control-plane <host:port>
mesh-ctl audit-log query --control-plane <host:port>
```

Старые endpoint-targeted rotation flags остаются для legacy paths, но v2
mesh-wide rotation проходит через control plane.

## Релизные проверки

Каждый release должен пройти automated critical suite, product emulation
playbook, Docker builds и RouterOS CHR gates до публикации тега.

```bash
go test -race -count=1 ./...
docker build -t awg-mesh-node:local -f deploy/Dockerfile.node .
docker build -t awg-mesh-client:local -f deploy/Dockerfile.client .
bash tests/critical/run-all.sh --strict
bash tests/simulation/F-009-CR-001-foundation-smoke.sh
bash tests/emulation-playbook/run.sh
BUILDX_BUILDER=default bash tests/simulation/mikrotik-chr-baseline-runtime.sh
BUILDX_BUILDER=default bash tests/simulation/mikrotik-version-matrix.sh
```

Release не завершён, пока annotated git tag не появился на origin и оба
реестра, GHCR и Docker Hub, не отдают matching `:vX.Y.Z` image tags. Полная
release policy описана в [AGENTS.md](AGENTS.md).

## Карта документации

| Документ | Назначение |
|---|---|
| [docs/PRODUCTION-TESTING-PLAYBOOK.md](docs/PRODUCTION-TESTING-PLAYBOOK.md) | Customer-mode product walkthrough. |
| [docs/MIKROTIK-VERSION-COMPAT.md](docs/MIKROTIK-VERSION-COMPAT.md) | Совместимость RouterOS generator/runtime. |
| [docs/MIGRATION.md](docs/MIGRATION.md) | Руководство по migration с v1.x на v2. |
| [docs/OPERATOR_FAQ.md](docs/OPERATOR_FAQ.md) | Operator details для tokens, backups и state files. |
| [docs/adr/README.md](docs/adr/README.md) | Architecture decision records. |

## Разработка

Сборка:

```bash
CGO_ENABLED=1 go build -trimpath -o bin/awg-mesh-node ./cmd/awg-mesh-node
CGO_ENABLED=1 go build -trimpath -o bin/mesh-ctl ./cmd/mesh-ctl
```

Тесты:

```bash
go test -race -count=1 ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 run ./...
```

Перегенерируйте protobuf files после изменения `proto/*.proto`:

```bash
protoc --proto_path=proto \
  --go_out=. --go_opt=module=github.com/coonfuuseed-paandaa/awg-mesh/v2 \
  --go-grpc_out=. --go-grpc_opt=module=github.com/coonfuuseed-paandaa/awg-mesh/v2 \
  proto/*.proto
```

## Лицензия

MIT. См. [LICENSE](LICENSE).
