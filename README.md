# Local Directory 数据源插件

主仓之外的 Linux/amd64 Go 插件，ID `community.local-directory`，版本 `0.1.0`，扩展 `local_directory`。只递归同步 Host 授权的 `/data/source`，不需要 WeKnora 专用注册代码。它不是独立 HTTP 服务，也不自行启动 Docker。

## 构建与测试

需要 Linux（`openat2`，内核 5.6+）、Go 1.26+。当前验证基线为 WeKnora
分支 `feat/framework-for-extending-capabilities` 的提交
`294f36c1efd3eb3100011ff530a29221719b001b` 及其未提交的阶段 0 原型清理。
SDK 尚未单独发布，因此这里不伪造可下载版本、不复制 Proto，也不把本机绝对
路径写入 go.mod。主仓开发指南位于
`website-docs/06-development/04-datasource-plugins.md`（从传给脚本的 WeKnora
仓库根目录读取）。

`scripts/with-sdk.sh` 在临时目录创建 go.work，将本项目和提供的 WeKnora 源码作为两个 workspace main module；退出后删除临时 workspace。首次下载普通 Go 依赖需要构建机网络，插件运行时不需要网络。单独 `GOWORK=off go build` 暂不支持；SDK 发布后才可改为真实版本依赖。当前直接依赖与主仓一致：grpc `v1.81.0`、protobuf `v1.36.11`、x/sys `v0.46.0`，SDK/Proto 仅来自上述本地真实提交。

```bash
# 在本项目目录执行；路径由开发者显式提供，不需要修改 go.mod。
bash scripts/with-sdk.sh /absolute/path/to/WeKnora go test -v ./...
bash scripts/with-sdk.sh /absolute/path/to/WeKnora go test -race ./...
bash scripts/build.sh /absolute/path/to/WeKnora
```

默认产物 `dist/community.local-directory/` 只有 `plugin.yaml` 和 `bin/plugin-linux-amd64`（CGO=0、trimpath、无 VCS stamping）。不含源码、.git、SDK 或缓存。输出目录已存在时拒绝覆盖；再次构建用第二个参数指定**新的绝对目录**。构建成功后重新 Discovery/Builder，不能覆盖已加载的 Artifact 快照。

## Manifest、配置与 SDK 映射

Manifest 使用 `plugins.weknora.io/v1alpha1`、`kind: Plugin`；兼容 WeKnora
`>=0.8.0 <0.9.0`。Protocol Version 与 DataSource Contract Version都为 `1.0`。
能力仅声明 `resource_listing`、`full_sync`、`incremental_sync`、`deletion_events`。

运行约束是 `sandbox_service` + `uds`、`network: none`、`filesystem: selected_directory_readonly`。显式资源预算：`cpuQuota: 0.5`、`memoryMiB: 256`、`maxProcesses: 32`。镜像、实例数、Gate、BPF、日志、tmpfs 和审计 sink 完全由现有 C1 Host 管理。

| 插件实现 | 现有 RPC |
|---|---|
| `sdk.NewControlServer(Identity, ControlHooks)` | Handshake、ValidateConfig、Health、Shutdown |
| `source.ListResources` | DataSourcePlugin.ListResources |
| `source.ResolveAncestors` | DataSourcePlugin.ResolveAncestors |
| `source.Sync` | DataSourcePlugin.Sync 流：Upsert、Delete、ItemError、Checkpoint |
| `sdk.ServeUDS` | 固定 `/run/weknora/plugin.sock`，无 TCP fallback |

固定 C1 bootstrap 只用于读取 Host 的启动 nonce；插件身份、版本和能力来自编译期常量。Health 只检查授权根是否可打开，不扫描文件。Shutdown/SIGTERM 通过 SDK 正常关闭；Host 仍负责限时停止与回收。

`configSchema` 是 `type: object, additionalProperties: false`，对应 Host DataSourceConfig 的 **settings**。RPC 接受现有完整配置信封：

```json
{"type":"local_directory","credentials":{},"resource_ids":["grant_root"],"settings":{}}
```

也接受 `{}` 和空选择；不接受设置 Host Path、网络地址、凭据或额外 settings。父 ID 为空时 ListResources 返回唯一资源：`grant_root / 已授权目录 / directory / HasChildren=false`；查询根的子资源返回空列表。根没有祖先。未知资源/父 ID 返回明确错误，不暴露 Host 路径。

## 文件、Diff 与提交规则

- `external_id` 是 `/` 分隔的规范相对路径；同路径更新 ID 不变，改名是旧路径 Delete + 新路径 Upsert。
- 只处理非空普通文件。安全打开使用 `openat2` 的 BENEATH/NO_SYMLINKS/NO_MAGICLINKS，以授权根 fd 为界；先用 O_PATH 检查 inode 类型，再从可信 `/proc/self/fd` 重开固定 inode，不按可被替换的源路径重开。拒绝 symlink（包括目录祖先）、设备、FIFO、Socket，以及非法文件名；没有不安全 fallback。
- 完整枚举目录，再按路径稳定排序逐文件读取、SHA-256。正文逐项发送，不同时缓存整个目录。Upsert 的 `content` 是**原始字节**，`file_name` 保留扩展名，`content_type` 由内容探测，`source_resource_id=grant_root`。PDF/二进制不会转换成字符串。现有 Adapter 将原始字节放入 FetchedItem.Content，Host 的文件解析/分块/Embedding/索引仍是后续业务职责。
- 比较文件内容；文件名/路径影响文档身份，mtime、扫描时间和文件权限本身不生成新版本。读前后检查 device/inode、mode、size、mtime、ctime，并复核路径仍指向该文件；目录枚举期间明显变化也失败重试。不提供整个目录的原子快照。
- 先成功读取全部当前文件、复核目录、确认新 Cursor 不超限，才根据旧快照输出 Delete。不可读、不安全条目不能被误认为删除；本轮 ItemError + 流错误，不输出最终 Checkpoint。
- 每轮仅有一个最终 Checkpoint，且必须在所有事件成功发送后。取消、读取/发送失败、非法 Cursor 都没有成功 Checkpoint。Checkpoint 发送成功不等于 Host 已持久接受；插件不保存内存外的独立同步数据库。

## Cursor / revision

插件私有状态位于现有 SyncCursor 的 `connector_cursor.local_directory`：

```json
{"connector_cursor":{"local_directory":{"version":1,"round":"1","files":{"a.txt":{"digest":"<sha256>","revision":"<sha256>"}}}}}
```

Round 用十进制字符串，避免 Host `map[string]interface{}` JSON 往返造成整数精度损失。每次成功扫描产生下一轮次；变化项的 revision 是 `SHA256("local-directory-v1\0" + nextRound + "\0" + path + "\0" + contentDigest)`，未变化项保留原 revision。这样相同旧 Cursor + 相同内容重放完全确定；A→B→A 和删除后重建不会被 Host 历史 `(data_source_id, external_id, revision)` 幂等键吞掉。此轮次与 Binding generation 无关。

空 Cursor 表示首次全量；有效 Cursor 做 Diff；`force_full` 携带有效旧 Cursor 时输出全部文件，但保留未变化项的 revision，不丢失轮次。未知格式/版本、损坏或超限 Cursor 明确失败，不悄悄当作首次同步。Grant/同步范围变化后的 Cursor 失效与业务重置由 C3 衔接，插件不猜测授权变化。

边界：每文件 `1..32 MiB`；最多 1024 文件、4096 条目（含根/目录）、深度 32、相对路径 512 UTF-8 字节。Cursor 接收上限 1 MiB，输出预留 1024 字节给 Host 信封重序列化；JSON 编码超限同样失败，不截断。配置上限 256 KiB；事件沿用 SDK 的 33 MiB gRPC 消息上限。不实现大文件分帧。空文件因当前 Host ingestion 拒绝空正文而明确报错；不能代表支持所有解析格式。

为兼容现有 Host multipart filename，还拒绝反斜杠、双引号、控制字符及非法 UTF-8 文件名。错误只包含插件可见的相对路径，不包含 Host 实际路径或凭据。

## 正式 Runtime 与应用验证

在带本轮薄集成入口的 WeKnora 主仓执行：

```bash
C2_ARTIFACT=/absolute/path/to/weknora-plugin-local-directory/dist/community.local-directory \
  bash scripts/test-plugin-backend.sh local-directory
```

复用 C1 受控 Controller 测试容器、固定运行镜像、安全 Gate/BPF、可信 Host audit sink 和显式 `/wk` AppPath ↔ 唯一 `/tmp/wkc1-*` HostPath 映射。仅测试容器需要 C1 已有 BPF/挂载能力与本机 Docker socket；插件始终无 Capability、禁网、非 root、只读授权。没有空审计实现。缺环境/产物显式失败；普通测试不要求存在此独立项目。

入口通过正式 Discovery → Artifact 快照 → Store/GrantService/Builder → Backend/Runtime（握手/校验/Health/发布）→ Binding-first Router/Adapter，验证真实授权目录与原始字节。小型接收端只保存不透明 Cursor 和核对事件，**没有创建 Knowledge，没有证明完整 WeKnora 入库**。停止后销毁实例，通过新 Runtime.Start 和新 nonce/Handle 重启，再原样传回旧 Cursor。旧 Stop 不能摘除新实例，旧 UDS 客户端不复用。

完整应用验收从 WeKnora 主仓正常 `cmd/server` 入口运行，使用隔离数据库、Redis、
deployment ID 和本地模型：

```bash
C2_ARTIFACT=/absolute/path/to/weknora-plugin-local-directory/dist/community.local-directory \
  bash scripts/test-plugin-application.sh
```

该入口经受鉴权 API 创建 DataSource、Binding 与 Grant，通过 QueuePlugin 把原始
文件送入现有解析、Embedding 和索引链，验证 active Knowledge、单文件增量、
应用重启恢复及可信审计。它要求本机 Docker、cgroup v2、bpffs、BTF、已预加载
的固定插件镜像及可用的本地 Ollama 模型；不会下载模型或提升权限。

清理由正式 Runtime.Stop/Backend 完成，检查本次容器、UDS/tmpfs、BPF pins 和
清理记录；脚本保留唯一临时证据目录并输出路径，使用者确认结果后可精确删除。
不要手工提前删除 pins/UDS，不运行全局 Docker prune。应用验收不重复完整的
CPU/OOM/PID 矩阵；宿主机直接运行 Controller 仍未实测，不构成本阶段的部署承诺。

常见错误：`managed C1 bootstrap required` 表示绕过了 Runtime；`operation not permitted` 可能是 Host BPF/挂载能力缺失，不得禁用策略；`unknown resource` 表示选择不合法；`source changed` 应保持旧 Cursor 重试；文件/目录/Cursor 边界错误需要调整源目录，而不是截断同步。

本项目是完整的递归只读目录示例；最小教学模板位于 WeKnora 主仓
`examples/plugins/datasource-go`。两者都不需要 Local Directory 专用主仓注册代码。
当前 V1 仍只支持单 Linux Controller、本机 Docker、UDS、`network:none` 和只读
目录，不包含热安装、市场、多节点或其他四类外部协议。
